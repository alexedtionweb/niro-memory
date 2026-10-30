package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func exchange(t *testing.T, s *Server, input string) []response {
	t.Helper()
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var responses []response
	decoder := json.NewDecoder(&out)
	for decoder.More() {
		var res response
		if err := decoder.Decode(&res); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, res)
	}
	return responses
}

func TestProtocol(t *testing.T) {
	s := &Server{}
	res := exchange(t, s, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":"tools","method":"tools/list"}
{"jsonrpc":"2.0","id":2,"method":"ping"}
{"jsonrpc":"2.0","id":3,"method":"unknown"}
invalid json
[]
{"jsonrpc":"2.0","id":null,"method":"ping"}
`)
	if len(res) != 7 {
		t.Fatalf("got %d responses: %+v", len(res), res)
	}
	if string(res[0].ID) != "0" || res[0].Result.(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %+v", res[0])
	}
	list := res[1].Result.(map[string]any)["tools"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["name"] != "search_memory" {
		t.Fatalf("tools: %+v", list)
	}
	for i, code := range []int{-32601, -32700, -32600, -32600} {
		if res[i+3].Error == nil || res[i+3].Error.Code != code {
			t.Fatalf("response %d: %+v", i+3, res[i+3])
		}
	}
}

func TestRememberSearchAndScope(t *testing.T) {
	ctx := context.Background()
	engine, err := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	if _, err := engine.Remember(ctx, niromem.Fact{
		Scope: memory.Scope{Tenant: "dev", User: "another-project"}, Text: "unrelated test command secret",
	}); err != nil {
		t.Fatal(err)
	}
	const key = "test-mcp-key-0123456789"
	auth, err := api.NewKeys([]api.Key{{Key: key, Tenant: "dev", User: "my-project", Agent: "claude-code"}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer((&api.Server{Engine: engine, Auth: auth}).Handler())
	defer httpServer.Close()
	c := client.New(httpServer.URL, key)
	s := &Server{client: c, user: "my-project", agent: "claude-code"}
	res := exchange(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"The test command is go test ./...","slot":"project.test_command"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"test command"}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"The test command is go test -race ./...","slot":"project.test_command"}}}
`)
	for _, response := range res {
		if response.Error != nil || response.Result.(map[string]any)["isError"] != false {
			t.Fatalf("tool failed: %+v", response)
		}
	}
	content := res[1].Result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, "go test ./...") || strings.Contains(content, "unrelated") {
		t.Fatalf("bad search result or scope leak: %s", content)
	}
	current, err := c.List(ctx, s.user, s.agent, 10)
	if err != nil || len(current) != 1 || current[0].Text != "The test command is go test -race ./..." {
		t.Fatalf("slot did not supersede: %+v %v", current, err)
	}
}

func TestToolFailuresAreExplicit(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"temporarily unavailable"}`))
	}))
	defer backend.Close()
	s := &Server{client: client.New(backend.URL, ""), user: "project"}
	res := exchange(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"tests"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":""}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"tests","user_id":"other"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing","arguments":{}}}
`)
	for _, response := range res[:3] {
		if response.Error != nil || response.Result.(map[string]any)["isError"] != true {
			t.Fatalf("failure masked: %+v", response)
		}
	}
	if res[3].Error == nil || res[3].Error.Code != -32602 {
		t.Fatalf("unknown tool: %+v", res[3])
	}
}
