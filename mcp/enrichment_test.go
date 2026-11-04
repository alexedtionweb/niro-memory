package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestMCPImmediateSaveRetryAndScopedEnrichment(t *testing.T) {
	store := memstore.New(nil)
	defer store.Close()
	engine, err := niromem.Open(niromem.Options{Store: store, Knowledge: niromem.KnowledgeOptions{
		Mode: "enforced", Planner: knowledge.Deterministic{}, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	httpServer := httptest.NewServer((&api.Server{Engine: engine, Auth: api.Dev{}}).Handler())
	defer httpServer.Close()
	c := client.New(httpServer.URL, "")
	s := &Server{client: c, user: "my-project", agent: "claude-code"}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"Atlas uses Redis.","operation_key":"mcp-source"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"Atlas uses Redis.","operation_key":"mcp-source"}}}
`
	res := exchange(t, s, input)
	for _, r := range res {
		if r.Error != nil || r.Result.(map[string]any)["isError"] != false {
			t.Fatalf("save failed: %+v", r)
		}
	}
	ms, err := engine.List(context.Background(), niromem.ListRequest{Tenant: "dev", As: memory.Principal{Admin: true}, User: "my-project", Agent: "claude-code", AllVersions: true})
	if err != nil || len(ms) != 1 {
		t.Fatalf("retry duplicated source: %+v %v", ms, err)
	}
	foreign, err := engine.Remember(context.Background(), niromem.Fact{Scope: memory.Scope{Tenant: "dev", User: "different-project"}, Text: "private data"})
	if err != nil {
		t.Fatal(err)
	}
	request := func(id string) string {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "inspect_enrichment", "arguments": map[string]string{"key": id}}})
		return string(b) + "\n"
	}
	good := exchange(t, s, request(ms[0].ID))[0]
	if good.Error != nil || good.Result.(map[string]any)["isError"] != false {
		t.Fatalf("status: %+v", good)
	}
	bad := exchange(t, s, request(foreign[0].Memory.ID))[0]
	if bad.Error != nil || bad.Result.(map[string]any)["isError"] != true {
		t.Fatalf("cross-project status leaked: %+v", bad)
	}
}
