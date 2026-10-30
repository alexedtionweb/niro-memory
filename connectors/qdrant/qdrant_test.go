package qdrant_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/connectors/qdrant"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	"github.com/alexedtionweb/niro-memory/vector"
	"github.com/alexedtionweb/niro-memory/vector/vectortest"
)

var n atomic.Int64

// Set NIROMEM_QDRANT_URL (e.g. http://localhost:6333) to run.
func TestConformance(t *testing.T) {
	url := os.Getenv("NIROMEM_QDRANT_URL")
	if url == "" {
		t.Skip("NIROMEM_QDRANT_URL not set")
	}
	vectortest.Run(t, func(t *testing.T) vector.Index {
		return qdrant.New(qdrant.Options{URL: url, Prefix: fmt.Sprintf("t%d_%d_", time.Now().UnixNano()%1e9, n.Add(1))})
	})
}

// TestEngineWithExternalIndex checks the outbox path: vectors are committed
// with an index job, a worker copies them to Qdrant, search uses Qdrant and
// re-checks hits against the store, and deletes are propagated.
func TestEngineWithExternalIndex(t *testing.T) {
	url := os.Getenv("NIROMEM_QDRANT_URL")
	if url == "" {
		t.Skip("NIROMEM_QDRANT_URL not set")
	}
	ctx := context.Background()
	idx := qdrant.New(qdrant.Options{URL: url, Prefix: fmt.Sprintf("e%d_", time.Now().UnixNano()%1e9)})
	e, _ := niromem.Open(niromem.Options{Store: memstore.New(nil), Vector: idx, Debounce: -1})
	defer e.Close()
	e.PutSettings(ctx, "t", niromem.Settings{VectorModels: []vector.Model{{ID: "m3", Dims: 3, Active: true}}})
	sc := memory.Scope{Tenant: "t", User: "u"}
	ch, err := e.Remember(ctx,
		niromem.Fact{Scope: sc, Text: "red apples", Vectors: []niromem.Vector{{Model: "m3", Values: []float32{1, 0, 0}}}},
		niromem.Fact{Scope: sc, Text: "blue sky", Vectors: []niromem.Vector{{Model: "m3", Values: []float32{0, 0, 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.Worker().RunOnce(ctx, 10); err != nil || n == 0 {
		t.Fatalf("index job: %d %v", n, err)
	}
	r, err := e.Search(ctx, niromem.SearchRequest{Tenant: "t", User: "u", Vector: &search.QueryVector{Model: "m3", Values: []float32{0.9, 0.1, 0}}, Limit: 1})
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Memory.Text != "red apples" {
		t.Fatalf("search via qdrant: %+v %v", r, err)
	}
	// Deleted in the store: invisible immediately (re-check), then removed from Qdrant.
	if err := e.Delete(ctx, "t", "test", ch[0].Memory.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = e.Search(ctx, niromem.SearchRequest{Tenant: "t", User: "u", Vector: &search.QueryVector{Model: "m3", Values: []float32{1, 0, 0}}, Limit: 5})
	for _, h := range r.Hits {
		if h.Memory.ID == ch[0].Memory.ID {
			t.Fatal("deleted memory returned from a stale index")
		}
	}
	e.Worker().RunOnce(ctx, 10)
	hits, _ := idx.Search(ctx, vector.Query{Tenant: "t", Model: "m3", Values: []float32{1, 0, 0}, K: 5})
	if len(hits) != 1 {
		t.Fatalf("unindex job: %+v", hits)
	}
}
