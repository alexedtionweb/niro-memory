package enginetest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
)

type testInferer struct{ calls atomic.Int64 }

func (p *testInferer) Name() string { return "fixture" }
func (p *testInferer) Infer(_ context.Context, in relations.Input) ([]relations.Proposal, error) {
	p.calls.Add(1)
	for _, m := range in.Candidates {
		if m.User != in.Source.User || m.Agent != in.Source.Agent || m.Visibility != in.Source.Visibility {
			return nil, errors.New("inference crossed owner/visibility")
		}
	}
	return []relations.Proposal{{Target: in.Candidates[0].ID, Relation: "supports", Reason: "Both discuss test conventions.", Confidence: 0.8}}, nil
}

func testRelationships(t *testing.T, factory Factory) {
	s := factory(t)
	defer s.Close()
	if _, ok := s.(relations.Store); !ok {
		t.Skip("optional relationship store not implemented")
	}
	provider := &testInferer{}
	e, err := niromem.Open(niromem.Options{Store: s, Relationships: provider, Debounce: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx := context.Background()
	scope := memory.Scope{Tenant: "relations", User: "project", Agent: "bot"}
	changes, err := e.Remember(ctx,
		niromem.Fact{Scope: scope, Text: "Use focused tests for connector changes"},
		niromem.Fact{Scope: scope, Text: "Connector test conventions use conformance"},
		niromem.Fact{Scope: memory.Scope{Tenant: "relations", User: "other"}, Text: "Private unrelated test conventions", Visibility: memory.Private})
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 0 {
		t.Fatal("inference ran inline on write")
	}
	if _, err := e.Worker().RunOnce(ctx, 20); err != nil {
		t.Fatal(err)
	}
	r, err := e.Explore(ctx, niromem.ListRequest{Tenant: "relations", User: "project", Agent: "bot"})
	if err != nil || len(r.Links) != 2 {
		t.Fatalf("derived links: %+v %v", r, err)
	}
	if err := e.BackfillRelationships(ctx, "relations"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Worker().RunOnce(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Worker().RunOnce(ctx, 20); err != nil {
		t.Fatal(err)
	}
	r, err = e.Explore(ctx, niromem.ListRequest{Tenant: "relations", User: "project", Agent: "bot"})
	if err != nil || len(r.Links) != 2 {
		t.Fatalf("backfill duplicated links: %+v %v", r, err)
	}
	other, err := e.Explore(ctx, niromem.ListRequest{Tenant: "other", As: memory.Principal{Admin: true}})
	if err != nil || len(other.Links) != 0 {
		t.Fatalf("cross-tenant links: %+v %v", other, err)
	}
	if err := e.Delete(ctx, "relations", "test", changes[0].Memory.ID); err != nil {
		t.Fatal(err)
	}
	r, err = e.Explore(ctx, niromem.ListRequest{Tenant: "relations", User: "project", Agent: "bot"})
	if err != nil || len(r.Links) != 0 {
		t.Fatalf("deleted evidence still linked: %+v %v", r, err)
	}
	stored, err := s.(relations.Store).RelationshipSets(ctx, "relations", []string{changes[0].Memory.ID, changes[1].Memory.ID})
	if err != nil || len(stored) != 0 {
		t.Fatalf("deleted evidence persisted in derived data: %+v %v", stored, err)
	}
}
