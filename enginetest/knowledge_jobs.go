package enginetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

func testKnowledgeGraphAndJobs(t *testing.T, factory Factory) {
	s := factory(t)
	defer s.Close()
	if _, ok := s.(relations.Store); !ok {
		t.Skip("optional relationships unavailable")
	}
	if _, ok := s.(store.Operations); !ok {
		t.Skip("optional receipts unavailable")
	}
	ctx := context.Background()
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{MaxGraphHops: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	sc := memory.Scope{Tenant: "graph-jobs", User: "alice"}
	changes, err := e.Remember(ctx,
		niromem.Fact{Scope: sc, Text: "Public source fact"},
		niromem.Fact{Scope: sc, Text: "Private dependent collection", Layer: memory.Scene, Visibility: memory.Private})
	if err != nil {
		t.Fatal(err)
	}
	source, dependent := changes[0].Memory, changes[1].Memory
	links := []relations.Link{
		{ID: "derivation", Tenant: sc.Tenant, Source: dependent.ID, Target: source.ID, Relation: "derived_from", Confidence: 1, Origin: "derived", At: time.Now()},
		{ID: "cycle", Tenant: sc.Tenant, Source: source.ID, Target: dependent.ID, Relation: "supports", Confidence: 1, Origin: "validated", At: time.Now()},
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: sc.Tenant, Relations: links}); err != nil {
		t.Fatal(err)
	}
	edges, err := e.KnowledgeRelations(ctx, niromem.RelationRequest{ListRequest: niromem.ListRequest{Tenant: sc.Tenant, As: memory.Principal{User: "bob"}},
		MemoryID: source.ID, Depth: 4})
	if err != nil || len(edges) != 0 {
		t.Fatalf("private graph leak: %+v %v", edges, err)
	}
	edges, err = e.KnowledgeRelations(ctx, niromem.RelationRequest{ListRequest: niromem.ListRequest{Tenant: sc.Tenant, As: memory.Principal{User: "alice"}},
		MemoryID: source.ID, Depth: 4})
	if err != nil || len(edges) != 2 {
		t.Fatalf("bounded cyclic graph: %+v %v", edges, err)
	}
	text := "Corrected public source"
	if _, err := e.Update(ctx, sc.Tenant, source.ID, niromem.Patch{Text: &text}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Worker().RunOnce(ctx, 20); err != nil {
		t.Fatal(err)
	}
	history, err := e.History(ctx, sc.Tenant, dependent.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications := 0
	for _, event := range history {
		if strings.Contains(event.Reason, "dependency_changed") {
			notifications++
		}
	}
	if notifications != 1 {
		t.Fatalf("want atomic dependency notification, got %+v", history)
	}
	current, err := e.Get(ctx, sc.Tenant, memory.Principal{User: "alice"}, dependent.ID)
	if err != nil || current.Text != dependent.Text || current.InvalidatedAt != nil {
		t.Fatalf("dependency re-evaluation destroyed original: %+v %v", current, err)
	}
	raw, err := json.Marshal(map[string]string{"id": dependent.ID, "source": source.ID})
	if err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: "same-crashed-job", Tenant: sc.Tenant, Kind: niromem.JobKnowledgeReevaluate, Payload: raw}
	handler := e.Worker().Handlers[niromem.JobKnowledgeReevaluate]
	for i := 0; i < 2; i++ {
		if err := handler(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	after, err := e.History(ctx, sc.Tenant, dependent.ID)
	if err != nil || len(after) != len(history)+1 {
		t.Fatalf("job replay duplicated audit event: %+v %v", after, err)
	}
}

func testKnowledgeConsolidation(t *testing.T, factory Factory) {
	s := factory(t)
	defer s.Close()
	if _, ok := s.(relations.Store); !ok {
		t.Skip("optional relationships unavailable")
	}
	if _, ok := s.(store.Operations); !ok {
		t.Skip("optional receipts unavailable")
	}
	ctx := context.Background()
	e, err := niromem.Open(niromem.Options{Store: s, Debounce: -1, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Consolidation: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	scope := memory.Scope{Tenant: "consolidation", User: "project"}
	_, err = e.Remember(ctx, niromem.Fact{Scope: scope, Text: "Use focused tests"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := e.Worker().RunOnce(ctx, 20); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := e.List(ctx, niromem.ListRequest{Tenant: scope.Tenant, User: scope.User, AllVersions: true})
	if err != nil || len(ms) != 3 {
		t.Fatalf("Atom -> Scene -> Profile: %+v %v", ms, err)
	}
	for _, m := range ms {
		if m.Layer == memory.Atom {
			continue
		}
		edges, err := e.KnowledgeRelations(ctx, niromem.RelationRequest{ListRequest: niromem.ListRequest{
			Tenant: scope.Tenant, User: scope.User, As: memory.Principal{User: scope.User}},
			MemoryID: m.ID, Direction: "out", Types: []string{"derived_from"}, Depth: 1})
		if err != nil || len(edges) != 1 {
			t.Fatalf("derived provenance: %+v %v", edges, err)
		}
	}
}
