package storetest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

func testTargetVectors(t *testing.T, s store.Store) {
	vt, ok := s.(store.VectorTargets)
	if !ok {
		t.Skip("no target vectors capability")
	}
	mt := func(id string) vector.Target { return vector.Target{Kind: vector.TargetMemory, ID: id} }
	pt := func(id string, part int) vector.Target {
		return vector.Target{Kind: vector.TargetPart, ID: id, Part: part}
	}
	point := func(model string, tg vector.Target, vals ...float32) vector.Point {
		return vector.Point{Model: model, Target: tg, Modality: "text", User: "u", Values: vals}
	}
	write(t, s, &store.Batch{Tenant: "t", Vectors: []vector.Point{
		point("b", mt("m1"), 0, 1),
		point("a", mt("m1"), 1, 0, 0),
		point("a", mt("m2"), 0, 0, 1),
		point("a", pt("e1", 2), 0, 1, 0),
		point("c", pt("e1", 0), 1, 0),
		point("a", mt("e1"), 1, 0, 0),
	}})
	write(t, s, &store.Batch{Tenant: "other", Vectors: []vector.Point{point("a", mt("m1"), 0, 1, 0), point("z", mt("m1"), 1, 0)}})

	got, err := vt.TargetVectors(ctx, "t", []vector.Target{mt("m1"), pt("e1", 2), mt("m1"), pt("e1", 0), mt("missing")})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		key, model string
		vals       []float32
	}{
		{"m:m1", "a", []float32{1, 0, 0}},
		{"m:m1", "b", []float32{0, 1}},
		{"p:e1:0", "c", []float32{1, 0}},
		{"p:e1:2", "a", []float32{0, 1, 0}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d points, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		p := got[i]
		if p.Tenant != "t" || p.Target.Key() != w.key || p.Model != w.model || p.Modality != "text" || p.User != "u" || fmt.Sprint(p.Values) != fmt.Sprint(w.vals) {
			t.Fatalf("point %d = %+v, want %+v", i, p, w)
		}
	}

	got[0].Values[0] = 42
	again, err := vt.TargetVectors(ctx, "t", []vector.Target{mt("m1")})
	if err != nil || len(again) != 2 || again[0].Values[0] != 1 {
		t.Fatalf("returned values alias storage: %+v %v", again, err)
	}

	if got, err := vt.TargetVectors(ctx, "other", []vector.Target{mt("m1"), mt("m2"), pt("e1", 2)}); err != nil || len(got) != 2 || got[0].Model != "a" || got[1].Model != "z" || got[0].Tenant != "other" {
		t.Fatalf("cross-tenant read %+v %v", got, err)
	}
	if got, err := vt.TargetVectors(ctx, "nobody", []vector.Target{mt("m1")}); err != nil || len(got) != 0 {
		t.Fatalf("unknown tenant %+v %v", got, err)
	}
	for _, empty := range [][]vector.Target{nil, {}} {
		if got, err := vt.TargetVectors(ctx, "t", empty); err != nil || len(got) != 0 {
			t.Fatalf("empty targets %+v %v", got, err)
		}
	}

	for name, bad := range map[string][]vector.Target{
		"empty id":      {{Kind: vector.TargetMemory}},
		"unknown kind":  {{Kind: "x", ID: "m1"}},
		"empty kind":    {{ID: "m1"}},
		"memory part":   {{Kind: vector.TargetMemory, ID: "m1", Part: 1}},
		"negative part": {{Kind: vector.TargetPart, ID: "e1", Part: -1}},
	} {
		if _, err := vt.TargetVectors(ctx, "t", bad); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := vt.TargetVectors(ctx, "", []vector.Target{mt("m1")}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("empty tenant: err = %v", err)
	}

	many := make([]vector.Target, 0, 2*store.MaxVectorTargets+1)
	for i := 0; i < store.MaxVectorTargets; i++ {
		many = append(many, mt(fmt.Sprintf("x%03d", i)), mt(fmt.Sprintf("x%03d", i)))
	}
	if _, err := vt.TargetVectors(ctx, "t", many); err != nil {
		t.Fatalf("%d distinct targets with duplicates: %v", store.MaxVectorTargets, err)
	}
	if _, err := vt.TargetVectors(ctx, "t", append(many, mt("one-more"))); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("oversize targets: err = %v", err)
	}

	// Fill exactly MaxTargetVectors rows for many targets, then exceed by one.
	models := store.MaxTargetVectors / store.MaxVectorTargets
	var fill []vector.Point
	for i := 0; i < store.MaxVectorTargets; i++ {
		for m := 0; m < models; m++ {
			fill = append(fill, point(fmt.Sprintf("bulk%02d", m), mt(fmt.Sprintf("x%03d", i)), 1, 0))
		}
	}
	write(t, s, &store.Batch{Tenant: "bulk", Vectors: fill})
	got, err = vt.TargetVectors(ctx, "bulk", many)
	if err != nil || len(got) != store.MaxTargetVectors || got[0].Target.ID != "x000" || got[0].Model != "bulk00" || got[len(got)-1].Target.ID != fmt.Sprintf("x%03d", store.MaxVectorTargets-1) {
		t.Fatalf("full bound: %d points %v", len(got), err)
	}
	write(t, s, &store.Batch{Tenant: "bulk", Vectors: []vector.Point{point("extra", mt("x050"), 0, 1)}})
	if got, err := vt.TargetVectors(ctx, "bulk", many); !errors.Is(err, store.ErrTooManyTargetVectors) || !errors.Is(err, memory.ErrInvalid) || got != nil {
		t.Fatalf("row bound not enforced: %d points %v", len(got), err)
	}
	if got, err := vt.TargetVectors(ctx, "bulk", []vector.Target{mt("x050")}); err != nil || len(got) != models+1 || got[models].Model != "extra" {
		t.Fatalf("narrow read after overflow: %d points %v", len(got), err)
	}
}
