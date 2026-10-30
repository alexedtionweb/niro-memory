// Package vectortest is the conformance kit for vector.Index connectors
// (Qdrant, OpenSearch, S3 Vectors, …):
//
//	func TestConformance(t *testing.T) {
//		vectortest.Run(t, func(t *testing.T) vector.Index { return newEmptyIndex(t) })
//	}
package vectortest

import (
	"context"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/vector"
)

// Factory returns a new, empty index.
type Factory func(t *testing.T) vector.Index

// Settle is how long to wait after writes for eventually consistent
// backends; set it before Run for such connectors.
var Settle time.Duration

func pt(tenant, model, id, user string, kind vector.TargetKind, part int, v ...float32) vector.Point {
	return vector.Point{Tenant: tenant, Model: model, Target: vector.Target{Kind: kind, ID: id, Part: part}, User: user, Values: v}
}

// Run executes the suite.
func Run(t *testing.T, newIndex Factory) {
	ctx := context.Background()
	t.Run("SearchScopesAndIsolation", func(t *testing.T) {
		idx := newIndex(t)
		err := idx.Upsert(ctx, []vector.Point{
			pt("a", "m", "1", "u1", vector.TargetMemory, 0, 1, 0, 0),
			pt("a", "m", "2", "u1", vector.TargetMemory, 0, 0.8, 0.6, 0),
			pt("a", "m", "3", "u2", vector.TargetMemory, 0, 0, 0, 1),
			pt("a", "m", "e1", "u1", vector.TargetPart, 2, 0.9, 0, 0.1),
			pt("b", "m", "x", "u1", vector.TargetMemory, 0, 1, 0, 0),
			pt("a", "other", "1", "u1", vector.TargetMemory, 0, 1, 0),
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(Settle)
		hits, err := idx.Search(ctx, vector.Query{Tenant: "a", Model: "m", Values: []float32{1, 0, 0}, K: 10})
		if err != nil || len(hits) != 4 || hits[0].Target.ID != "1" || hits[0].Score < 0.99 {
			t.Fatalf("search: %+v %v", hits, err)
		}
		for i := 1; i < len(hits); i++ {
			if hits[i].Score > hits[i-1].Score {
				t.Fatal("hits must be sorted by score")
			}
			if hits[i].Target.ID == "x" {
				t.Fatal("tenant leak")
			}
		}
		if h, _ := idx.Search(ctx, vector.Query{Tenant: "a", Model: "m", Values: []float32{1, 0, 0}, K: 10, User: "u2"}); len(h) != 1 || h[0].Target.ID != "3" {
			t.Fatalf("user filter: %+v", h)
		}
		h, _ := idx.Search(ctx, vector.Query{Tenant: "a", Model: "m", Values: []float32{1, 0, 0}, K: 10, Kind: vector.TargetPart})
		if len(h) != 1 || h[0].Target.ID != "e1" || h[0].Target.Part != 2 {
			t.Fatalf("part targets: %+v", h)
		}
		if h, _ := idx.Search(ctx, vector.Query{Tenant: "a", Model: "m", Values: []float32{1, 0, 0}, K: 2}); len(h) != 2 {
			t.Fatal("k")
		}
	})
	t.Run("UpsertReplacesAndDeleteRemoves", func(t *testing.T) {
		idx := newIndex(t)
		idx.Upsert(ctx, []vector.Point{pt("a", "m", "1", "", vector.TargetMemory, 0, 1, 0), pt("a", "m", "2", "", vector.TargetMemory, 0, 0, 1)})
		idx.Upsert(ctx, []vector.Point{pt("a", "m", "1", "", vector.TargetMemory, 0, 0, 1)})
		time.Sleep(Settle)
		h, _ := idx.Search(ctx, vector.Query{Tenant: "a", Model: "m", Values: []float32{1, 0}, K: 5})
		if len(h) != 2 || h[0].Score > 0.01 {
			t.Fatalf("upsert must replace: %+v", h)
		}
		if err := idx.Delete(ctx, "a", []vector.Target{{Kind: vector.TargetMemory, ID: "1"}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(Settle)
		if h, _ := idx.Search(ctx, vector.Query{Tenant: "a", Model: "m", Values: []float32{0, 1}, K: 5}); len(h) != 1 || h[0].Target.ID != "2" {
			t.Fatalf("delete: %+v", h)
		}
	})
}
