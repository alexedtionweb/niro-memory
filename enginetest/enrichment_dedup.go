package enginetest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

type dedupExtractor struct {
	knowledge.Deterministic
	ready   chan struct{}
	release chan struct{}
}

func (p dedupExtractor) Enrich(_ context.Context, in knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	if p.ready != nil {
		p.ready <- struct{}{}
		<-p.release
	}
	quote := strings.TrimSpace(in.Text)
	if strings.Contains(in.Text, "Atlas uses Redis.") {
		quote = "Atlas uses Redis."
	}
	return knowledge.Enrichment{
		Facts:     []knowledge.ExtractedFact{{Quote: quote, Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact}},
		Entities:  []knowledge.ExtractedEntity{{Name: "Atlas", Type: "project", Quote: quote}, {Name: "Redis", Type: "database", Quote: quote}},
		Relations: []knowledge.ExtractedRelation{},
	}, nil
}

type entityOnlyExtractor struct{ knowledge.Deterministic }

func (entityOnlyExtractor) Enrich(_ context.Context, in knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	return knowledge.Enrichment{Facts: []knowledge.ExtractedFact{}, Relations: []knowledge.ExtractedRelation{}, Entities: []knowledge.ExtractedEntity{
		{Name: "Alice", Type: "person", Quote: "Alice owns Atlas.", Attributes: map[string]any{"role": "owner"}},
		{Name: "Atlas", Type: "project", Quote: "Alice owns Atlas."},
	}}, nil
}

func testEntityOnlyEnrichment(t *testing.T, factory Factory) {
	ctx := context.Background()
	s := factory(t)
	defer s.Close()
	profile := knowledge.DefaultExtractionProfile()
	profile.EntityAttributes = map[string]knowledge.AttributeSchema{"role": {Type: "string"}}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: entityOnlyExtractor{}, BackgroundEnrichment: true, Extraction: &profile}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "entities", User: "u"}, Text: "Alice owns Atlas."})
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].Memory.ID
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "entities", Kind: niromem.JobKnowledgeEnrich, Key: id})
	if err != nil || len(js) != 1 {
		t.Fatal(err)
	}
	if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	result, err := e.EnrichmentStatus(ctx, "entities", memory.Principal{User: "u"}, id)
	if err != nil || len(result.Changes) != 2 || len(result.Extraction.Entities) != 2 || result.Extraction.Entities[0].Attributes["role"] != "owner" {
		t.Fatalf("entity-only extraction missing: %+v %v", result, err)
	}
	entities, err := s.(store.Entities).FindEntities(ctx, "entities", []string{memory.EntityKey("Alice"), memory.EntityKey("Atlas")})
	if err != nil || len(entities) != 2 {
		t.Fatalf("unindexed entity-only result: %+v %v", entities, err)
	}
	for _, entity := range entities {
		if entity.Type != "person" && entity.Type != "project" {
			t.Fatalf("untyped entity: %+v", entity)
		}
	}
}

func testEnrichmentDedupLifecycle(t *testing.T, factory Factory) {
	ctx := context.Background()
	s := factory(t)
	defer s.Close()
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: dedupExtractor{}, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	save := func(text string) niromem.EnrichmentResult {
		t.Helper()
		changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "life", User: "u"}, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		source := changes[0].Memory.ID
		js, err := s.Jobs(ctx, store.JobQuery{Tenant: "life", Kind: niromem.JobKnowledgeEnrich, Key: source})
		if err != nil || len(js) != 1 {
			t.Fatal(err)
		}
		if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, js[0]); err != nil {
			t.Fatal(err)
		}
		result, err := e.EnrichmentStatus(ctx, "life", memory.Principal{User: "u"}, source)
		if err != nil || len(result.Changes) != 1 {
			t.Fatalf("result: %+v %v", result, err)
		}
		return *result
	}
	first := save("Atlas uses Redis. First evidence.")
	id := first.Changes[0].Memory.ID
	second := save("Atlas uses Redis. Independent evidence.")
	if second.Changes[0].Memory.ID != id || second.Changes[0].Action != memory.ActionEvidence {
		t.Fatal("independent evidence created duplicate assertion")
	}
	ms, err := s.Get(ctx, "life", []string{id})
	if err != nil || len(ms) != 1 {
		t.Fatal(err)
	}
	if ms[0].Version != 1 || ms[0].Text != first.Changes[0].Memory.Text || !reflect.DeepEqual(ms[0].Source, first.Changes[0].Memory.Source) {
		t.Fatal("reinforcement changed original provenance")
	}
	events, err := s.History(ctx, "life", id)
	if err != nil || len(events) != 2 || events[1].Action != memory.ActionEvidence {
		t.Fatalf("reinforcement is not auditable: %+v %v", events, err)
	}
	if err := e.Invalidate(ctx, "life", id, time.Now(), "test", "withdraw"); err != nil {
		t.Fatal(err)
	}
	third := save("Atlas uses Redis. Later evidence.")
	if third.Changes[0].Memory.ID == id || third.Changes[0].Action != memory.ActionAdd {
		t.Fatal("closed assertion reused")
	}
	newID := third.Changes[0].Memory.ID
	if err := e.Delete(ctx, "life", "test", newID); err != nil {
		t.Fatal(err)
	}
	fourth := save("Atlas uses Redis. After deletion.")
	if fourth.Changes[0].Memory.ID == newID || fourth.Changes[0].Memory.ID == id {
		t.Fatal("deleted or closed assertion resurrected")
	}
}

func testEnrichmentDedup(t *testing.T, factory Factory) {
	ctx := context.Background()
	s := factory(t)
	defer s.Close()
	p := dedupExtractor{ready: make(chan struct{}, 2), release: make(chan struct{})}
	opts := niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: p, BackgroundEnrichment: true}}
	first, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	scope := memory.Scope{Tenant: "dedup", User: "project"}
	var sources []memory.Memory
	for _, text := range []string{"Atlas uses Redis.", "Atlas uses Redis!"} {
		changes, err := first.Remember(ctx, niromem.Fact{Scope: scope, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, changes[0].Memory)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: scope.Tenant, Kind: niromem.JobKnowledgeEnrich})
	if err != nil || len(js) != 2 {
		t.Fatalf("source jobs: %d %v", len(js), err)
	}
	engines := []*niromem.Engine{first, second}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, j := range js {
		wg.Add(1)
		go func(i int, j store.Job) {
			defer wg.Done()
			err := engines[i].Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, j)
			if errors.Is(err, memory.ErrConflict) {
				err = engines[i].Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, j)
			}
			errs <- err
		}(i, j)
	}
	<-p.ready
	<-p.ready
	close(p.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := first.EnrichmentStatus(ctx, scope.Tenant, memory.Principal{User: scope.User}, sources[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := first.EnrichmentStatus(ctx, scope.Tenant, memory.Principal{User: scope.User}, sources[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Changes) != 1 || len(b.Changes) != 1 || a.Changes[0].Memory.ID != b.Changes[0].Memory.ID {
		t.Fatalf("concurrent assertions duplicated: %+v %+v", a, b)
	}
	if a.Changes[0].Action == b.Changes[0].Action {
		t.Fatal("did not record distinct add and reinforcement")
	}
	id := a.Changes[0].Memory.ID
	ms, err := s.Find(ctx, store.Query{Tenant: scope.Tenant, AllVersions: true})
	if err != nil || len(ms) != 3 {
		t.Fatalf("assertion count: %d %v", len(ms), err)
	}
	targets, err := s.Get(ctx, scope.Tenant, []string{id})
	if err != nil || len(targets) != 1 {
		t.Fatal(err)
	}
	original := targets[0]
	links, err := s.(relations.Store).RelationshipSets(ctx, scope.Tenant, []string{id})
	if err != nil || len(links) != 2 {
		t.Fatalf("lost source support: %+v %v", links, err)
	}
	for _, j := range js {
		if err := first.Worker().Handlers[niromem.JobKnowledgeEnrich](ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	again, err := s.Get(ctx, scope.Tenant, []string{id})
	if err != nil || !reflect.DeepEqual(original, again[0]) {
		t.Fatal("retry rewrote immutable assertion or evidence")
	}
	links, err = s.(relations.Store).RelationshipSets(ctx, scope.Tenant, []string{id})
	if err != nil || len(links) != 2 {
		t.Fatal("retry duplicated evidence links")
	}
}
