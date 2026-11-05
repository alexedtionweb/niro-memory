package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	"github.com/alexedtionweb/niro-memory/vector"
)

type localQueryEmbedder struct{ embed.Hash }

func (e localQueryEmbedder) Embed(ctx context.Context, purpose embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	if purpose == embed.Query {
		select {
		case <-time.After(120 * time.Millisecond):
		case <-ctx.Done():
			return nil, embed.Usage{}, ctx.Err()
		}
	}
	return e.Hash.Embed(ctx, purpose, in)
}

type deadlineBackend struct {
	Backend
	t *testing.T
}

func (b deadlineBackend) Search(ctx context.Context, req client.SearchRequest) (*search.Result, error) {
	b.checkDeadline(ctx, SearchTimeout)
	return b.Backend.Search(ctx, req)
}

func (b deadlineBackend) Remember(ctx context.Context, facts ...niromem.Fact) ([]pipeline.Change, error) {
	b.checkDeadline(ctx, 5*time.Second)
	return b.Backend.Remember(ctx, facts...)
}

func (b deadlineBackend) checkDeadline(ctx context.Context, want time.Duration) {
	b.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < want-time.Second || time.Until(deadline) > want {
		b.t.Fatalf("unexpected tool deadline: %s, want %s", time.Until(deadline), want)
	}
}

func TestMCPSearchWaitsForLocalEmbeddingAndReturnsVectorMatch(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	e := localQueryEmbedder{Hash: embed.Hash{Dims: 64}}
	engine, err := niromem.Open(niromem.Options{Store: s, Embedder: e})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	changes, err := engine.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "dev", User: "project"}, Text: "An assertion without overlapping vocabulary."})
	if err != nil {
		t.Fatal(err)
	}
	values, _, err := e.Hash.Embed(ctx, embed.Query, []embed.Input{{Text: "semantic recall"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Write(ctx, &store.Batch{Tenant: "dev", Vectors: []vector.Point{{
		Model: e.Model().ID, Target: vector.Target{Kind: vector.TargetMemory, ID: changes[0].Memory.ID}, User: "project", Values: values[0],
	}}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer((&api.Server{Engine: engine, Auth: api.Dev{}}).Handler())
	defer httpServer.Close()
	server := &Server{client: deadlineBackend{Backend: client.New(httpServer.URL, ""), t: t}, user: "project"}
	responses := exchange(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"semantic recall"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"A separate approved fact."}}}
`)
	for _, response := range responses {
		if response.Error != nil || response.Result.(map[string]any)["isError"] != false {
			t.Fatalf("tool failed: %+v", response)
		}
	}
	content := responses[0].Result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var result search.Result
	if err := json.Unmarshal([]byte(content), &result); err != nil || len(result.Hits) != 1 || result.Hits[0].Memory.ID != changes[0].Memory.ID {
		t.Fatalf("MCP lost vector-only result: %s %v", content, err)
	}
	found := false
	for _, step := range result.Trace {
		if step.Strategy == "vector" && step.Ran {
			found = true
		}
	}
	if !found {
		t.Fatal("MCP returned without completing vector retrieval")
	}
}
