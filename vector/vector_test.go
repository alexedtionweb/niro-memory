package vector

import (
	"context"
	"math"
	"testing"
)

func TestNormalizeDotTopK(t *testing.T) {
	v := []float32{3, 4}
	if err := Normalize(v); err != nil || math.Abs(float64(Dot(v, v))-1) > 1e-6 {
		t.Fatalf("%v %v", v, err)
	}
	if Normalize([]float32{0, 0}) == nil {
		t.Fatal("zero vector accepted")
	}
	top := NewTopK(2)
	for i, s := range []float64{0.1, 0.9, 0.5, 0.7} {
		top.Push(Hit{Target: Target{ID: string(rune('a' + i))}, Score: s})
	}
	got := top.Sorted()
	if len(got) != 2 || got[0].Score != 0.9 || got[1].Score != 0.7 {
		t.Fatalf("%+v", got)
	}
}

func TestFlat(t *testing.T) {
	ctx := context.Background()
	f := NewFlat()
	f.Upsert(ctx, []Point{
		{Tenant: "t", Model: "m", Target: Target{Kind: TargetMemory, ID: "a"}, User: "u", Values: []float32{1, 0}},
		{Tenant: "t", Model: "m", Target: Target{Kind: TargetPart, ID: "e", Part: 1}, Values: []float32{0, 1}},
		{Tenant: "x", Model: "m", Target: Target{Kind: TargetMemory, ID: "z"}, Values: []float32{1, 0}},
	})
	hits, _ := f.Search(ctx, Query{Tenant: "t", Model: "m", Values: []float32{1, 0.1}, K: 5})
	if len(hits) != 2 || hits[0].Target.ID != "a" {
		t.Fatalf("%+v", hits)
	}
	if h, _ := f.Search(ctx, Query{Tenant: "t", Model: "m", Values: []float32{1, 0}, K: 5, Kind: TargetPart}); len(h) != 1 || h[0].Target.Key() != "p:e:1" {
		t.Fatalf("%+v", h)
	}
	if _, err := f.Search(ctx, Query{Tenant: "t", Model: "m", Values: []float32{1, 0, 0}, K: 1}); err != ErrDims {
		t.Fatal("dims")
	}
	f.Delete(ctx, "t", []Target{{Kind: TargetMemory, ID: "a"}})
	if h, _ := f.Search(ctx, Query{Tenant: "t", Model: "m", Values: []float32{1, 0}, K: 5}); len(h) != 1 {
		t.Fatal("delete")
	}
}

func BenchmarkDot1536(b *testing.B) {
	x, y := make([]float32, 1536), make([]float32, 1536)
	for i := range x {
		x[i], y[i] = float32(i%7), float32(i%5)
	}
	for i := 0; i < b.N; i++ {
		Dot(x, y)
	}
}
