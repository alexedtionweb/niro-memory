package niromem_test

import (
	"context"
	"errors"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type extractiveFixture struct {
	extractionFixture
	output knowledge.Enrichment
}

func (p *extractiveFixture) Enrich(context.Context, knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	p.calls++
	return p.output, p.fail
}

func TestExtractiveEnrichmentCommitsTextOnlyOrReviewWithoutBlockingEmbeddings(t *testing.T) {
	for _, scenario := range []string{"text-only", "shadow", "review"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := memstore.New(nil)
			defer s.Close()
			p := &extractiveFixture{output: knowledge.Enrichment{Facts: []knowledge.ExtractedFact{}, Entities: []knowledge.ExtractedEntity{}, Relations: []knowledge.ExtractedRelation{},
				Excerpts: []string{"Atlas uses Redis."}, Provider: "test", Model: "small"}}
			mode := "enforced"
			if scenario == "shadow" {
				mode = "shadow"
			}
			if scenario == "review" {
				p.fail = &knowledge.ExtractionReviewRequired{Err: errors.New("invalid selection")}
			}
			e, err := niromem.Open(niromem.Options{Store: s, Embedder: embed.Hash{Dims: 8}, Knowledge: niromem.KnowledgeOptions{Mode: mode, Planner: p, BackgroundEnrichment: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			changes, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas uses Redis. Bruna works at Contoso.", Visibility: memory.Private})
			if err != nil || len(changes) != 1 {
				t.Fatalf("save: %+v %v", changes, err)
			}
			id := changes[0].Memory.ID
			rows, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: niromem.JobKnowledgeEnrich, Key: id})
			if err != nil || len(rows) != 1 {
				t.Fatalf("job: %+v %v", rows, err)
			}
			handler := e.Worker().Handlers[niromem.JobKnowledgeEnrich]
			for i := 0; i < 2; i++ {
				if err := handler(ctx, rows[0]); err != nil {
					t.Fatal(err)
				}
			}
			result, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "u"}, id)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := "completed"
			if scenario == "shadow" {
				wantStatus = "shadow"
			}
			if scenario == "review" {
				wantStatus = "needs_review"
			}
			if result.Status != wantStatus || p.calls != 1 || len(result.Relations) != 0 {
				t.Fatalf("unexpected result/replay: %+v calls=%d", result, p.calls)
			}
			if scenario == "text-only" {
				if len(result.Changes) != 1 || result.Changes[0].Memory.Text != "Atlas uses Redis." {
					t.Fatalf("missing source excerpt: %+v", result)
				}
				derived := result.Changes[0].Memory
				a, err := memory.ReadAssertion(derived)
				if err != nil || a != nil || derived.Subject != "" || derived.Predicate != "" || derived.Object != "" || len(derived.Entities) != 0 || derived.Visibility != memory.Private || derived.User != "u" {
					t.Fatalf("text acquired authority or lost ACL: %+v %v", derived, err)
				}
				meta := derived.Metadata["enrichment"].(map[string]any)
				if meta["source_id"] != id {
					t.Fatal("excerpt lost source provenance")
				}
			} else if len(result.Changes) != 0 {
				t.Fatal("shadow or review applied knowledge changes")
			}
			if scenario == "review" && (result.Error == "" || result.Extraction != nil) {
				t.Fatalf("failure disguised as successful extraction: %+v", result)
			}
			if _, err := e.EnrichmentStatus(ctx, "t", memory.Principal{User: "other"}, id); err == nil {
				t.Fatal("review/extraction leaked across owner")
			}
			if err := e.Worker().Handlers[niromem.JobEmbed](ctx, store.Job{Tenant: "t"}); err != nil {
				t.Fatal(err)
			}
			have, total, err := e.Coverage(ctx, "t", embed.Hash{Dims: 8}.Model().ID)
			if err != nil || total == 0 || have != total {
				t.Fatalf("embedding blocked by extraction: %d/%d %v", have, total, err)
			}
			original, err := s.Get(ctx, "t", []string{id})
			if err != nil || len(original) != 1 || original[0].Text != "Atlas uses Redis. Bruna works at Contoso." {
				t.Fatal("original text changed or disappeared")
			}
		})
	}
}
