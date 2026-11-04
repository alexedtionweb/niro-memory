package enginetest

import (
	"context"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

type groundedExtractor struct{ knowledge.Deterministic }

func (groundedExtractor) Enrich(_ context.Context, in knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	out := knowledge.Enrichment{Facts: []knowledge.ExtractedFact{{Quote: in.Text, Subject: "Atlas", Predicate: "does not use", Object: "Redis", Type: memory.EpistemicFact}},
		Relations: []knowledge.ExtractedRelation{}, Provider: "fixture", Model: "cpu"}
	for i, m := range in.Candidates {
		if m.Text == "Atlas uses Redis." {
			out.Relations = append(out.Relations, knowledge.ExtractedRelation{Fact: 0, Candidate: i, Relation: "contradicts"})
		}
	}
	return out, nil
}

func testEnrichment(t *testing.T, factory Factory) {
	s := factory(t)
	defer s.Close()
	ctx := context.Background()
	opts := niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: groundedExtractor{}, BackgroundEnrichment: true}}
	first, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	scope := memory.Scope{Tenant: "enrichment", User: "project"}
	if _, err := first.Remember(ctx, niromem.Fact{Scope: scope, Text: "Atlas uses Redis."}); err != nil {
		t.Fatal(err)
	}
	changes, err := first.Remember(ctx, niromem.Fact{Scope: scope, Text: "Atlas does not use Redis.", OperationKey: "source"})
	if err != nil {
		t.Fatal(err)
	}
	source := changes[0].Memory
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: scope.Tenant, Kind: niromem.JobKnowledgeEnrich, Key: source.ID})
	if err != nil || len(js) != 1 {
		t.Fatalf("durable source outbox: %+v %v", js, err)
	}
	second, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	if err := first.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	result, err := first.EnrichmentStatus(ctx, scope.Tenant, memory.Principal{User: scope.User}, source.ID)
	if err != nil || result.Status != "completed" || len(result.Changes) != 1 || len(result.Relations) != 2 {
		t.Fatalf("multi-node extraction: %+v %v", result, err)
	}
	links, err := s.(relations.Store).RelationshipSets(ctx, scope.Tenant, []string{result.Changes[0].Memory.ID})
	if err != nil || len(links) != 2 {
		t.Fatalf("atomic evidence/conflict edges: %+v %v", links, err)
	}
	ms, err := s.Find(ctx, store.Query{Tenant: scope.Tenant, AllVersions: true})
	if err != nil || len(ms) != 3 {
		t.Fatalf("duplicate transitions: %d %v", len(ms), err)
	}
	// Closing evidence is itself atomic with dependency notifications. The
	// original assertion remains readable; no inferred correction is applied.
	if err := first.Invalidate(ctx, scope.Tenant, source.ID, source.ValidFrom, "fixture", "withdrawn"); err != nil {
		t.Fatal(err)
	}
	deps, err := s.Jobs(ctx, store.JobQuery{Tenant: scope.Tenant, Kind: niromem.JobKnowledgeReevaluate})
	if err != nil || len(deps) == 0 {
		t.Fatalf("missing dependency notifications: %+v %v", deps, err)
	}
}
