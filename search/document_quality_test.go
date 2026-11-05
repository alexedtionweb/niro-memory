package search

import (
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
)

func TestDocumentRankingIgnoresImportRecencyAndDemotesNavigation(t *testing.T) {
	s := &Searcher{}
	now := time.Now()
	r := Request{Policy: Default["default"], Explain: true}
	base := memory.Memory{ID: "content", Text: "Confirm installation before activating.", Confidence: 1, Importance: .5,
		Source: memory.Source{Kind: "document"}, ValidFrom: now}
	a := s.hit(base, &cand{}, .7, nil, r, now)
	base.ValidFrom = now.Add(-365 * 24 * time.Hour)
	b := s.hit(base, &cand{}, .7, nil, r, now)
	if a.Score != b.Score {
		t.Fatal("import time altered document relevance")
	}
	base.Metadata = map[string]any{"navigation_only": true}
	nav := s.hit(base, &cand{}, .75, nil, r, now)
	if nav.Score >= a.Score || nav.Explain.ContentWeight != .5 {
		t.Fatal("link-only navigation outranked useful content")
	}
	base.Source.Kind = "explicit"
	base.Metadata = nil
	old := s.hit(base, &cand{}, .7, nil, r, now)
	base.ValidFrom = now
	recent := s.hit(base, &cand{}, .7, nil, r, now)
	if recent.Score <= old.Score {
		t.Fatal("non-document recency behavior changed")
	}
}

func TestCollapseExtractiveCopiesOnlyWhenAuthorizedOriginalIsPresent(t *testing.T) {
	source := Hit{Memory: memory.Memory{ID: "source", Text: "Atlas stores data in Redis. Other context."}}
	copy := Hit{Memory: memory.Memory{ID: "copy", Text: "Atlas stores data in Redis.", Source: memory.Source{Kind: "knowledge_enriched"},
		Metadata: map[string]any{"enrichment": map[string]any{"source_id": "source", "mode": "extractive"}}}}
	other := Hit{Memory: memory.Memory{ID: "other", Text: "Independent relevant result."}}
	out, removed := collapseDerivedPassages([]Hit{copy, source, other})
	if len(out) != 2 || removed != 1 || out[0].Memory.ID != "source" {
		t.Fatalf("copy not collapsed: %+v", out)
	}
	out, removed = collapseDerivedPassages([]Hit{copy, other})
	if len(out) != 2 || removed != 0 {
		t.Fatal("copy lost without its accessible original")
	}
	copy.Memory.Subject = "Atlas"
	out, removed = collapseDerivedPassages([]Hit{copy, source})
	if len(out) != 2 || removed != 0 {
		t.Fatal("structured assertion was collapsed")
	}
}
