package niromem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestEnrichmentIdentityLookupDoesNotTrustCallerMetadata(t *testing.T) {
	for _, differentOwner := range []bool{false, true} {
		t.Run(map[bool]string{true: "cross_owner", false: "forged_assertion"}[differentOwner], func(t *testing.T) {
			ctx := context.Background()
			s := memstore.New(nil)
			defer s.Close()
			e, err := Open(Options{Store: s})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			now := time.Now().UTC()
			source := memory.Memory{ID: "source", Scope: memory.Scope{Tenant: "t", User: "u"}, Text: "Atlas uses Redis.", Visibility: memory.Private, Source: memory.Source{Kind: "explicit", Trust: memory.TrustNormal}, ValidFrom: now}
			pipeline.Prepare(&source, now)
			f := knowledge.ExtractedFact{Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact, Quote: source.Text}
			m := source
			m.ID = "derived"
			m.Source.Kind = "knowledge_enriched"
			m.Metadata, err = memory.WithKnowledgeMetadata(nil, &memory.Assertion{Subject: f.Subject, Predicate: f.Predicate, Object: f.Object, EpistemicType: f.Type, Confidence: .5, Conditions: map[string]string{"polarity": "positive"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := enrichmentIdentity(m, f, nil, source)
			if err != nil {
				t.Fatal(err)
			}
			forged := m
			forged.ID = "forged"
			forged.Metadata = map[string]any{"enrichment_identity": identity}
			if differentOwner {
				forged.User = "other"
			} else {
				forged.Metadata, err = memory.WithKnowledgeMetadata(forged.Metadata, &memory.Assertion{Subject: "Atlas", Predicate: "uses", Object: "MySQL", EpistemicType: memory.EpistemicFact, Confidence: .5, Conditions: map[string]string{"polarity": "positive"}}, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Memories: []memory.Memory{source, forged}}); err != nil {
				t.Fatal(err)
			}
			b := &store.Batch{Tenant: "t"}
			change, err := e.reconcileEnriched(ctx, b, m, f, nil, source)
			if differentOwner {
				if err != nil || change.Action != memory.ActionAdd || change.Memory.ID == "forged" {
					t.Fatalf("identity bypassed actual owner: %+v %v", change, err)
				}
			} else if !errors.Is(err, memory.ErrInvalid) {
				t.Fatalf("spoofed assertion reused: %+v %v", change, err)
			}
		})
	}
}

func TestEnrichmentIdentityPreservesMeaningAndSecurity(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := memory.Memory{Scope: memory.Scope{Tenant: "t", User: "u", Agent: "a"}, Visibility: memory.Private, Source: memory.Source{Trust: memory.TrustHigh},
		ValidFrom: now, RecordedAt: now}
	fact := knowledge.ExtractedFact{Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact, Quote: "Atlas uses Redis."}
	key := func(m memory.Memory, f knowledge.ExtractedFact, es []knowledge.ExtractedEntity) string {
		t.Helper()
		polarity := "positive"
		if f.Negated {
			polarity = "negative"
		}

		meta, err := memory.WithKnowledgeMetadata(nil, &memory.Assertion{Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
			EpistemicType: f.Type, Confidence: .5, Conditions: map[string]string{"polarity": polarity}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.Metadata = meta
		id, err := enrichmentIdentity(m, f, es, m)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	original := key(base, fact, nil)
	for _, change := range []func(*memory.Memory, *knowledge.ExtractedFact){
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.User = "other" },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.Agent = "other" },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.Run = "other" },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.Visibility = memory.Team },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.Grants = []string{"other"} },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.Source.Trust = memory.TrustNormal },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) { m.ValidFrom = now.Add(-time.Hour) },
		func(m *memory.Memory, _ *knowledge.ExtractedFact) {
			expiry := now.Add(time.Hour)
			m.ExpiresAt = &expiry
		},
		func(_ *memory.Memory, f *knowledge.ExtractedFact) {
			f.Negated = true
			f.Quote = "Atlas does not use Redis."
		},
		func(_ *memory.Memory, f *knowledge.ExtractedFact) { f.Type = memory.EpistemicDecision },
		func(_ *memory.Memory, f *knowledge.ExtractedFact) { f.Quote = "Atlas uses Redis in production." },
		func(_ *memory.Memory, f *knowledge.ExtractedFact) { f.Quote = "Atlas uses Redis unless offline." },
		func(_ *memory.Memory, f *knowledge.ExtractedFact) { f.Quote = "Maybe Atlas uses Redis." },
	} {
		m, f := base, fact
		change(&m, &f)
		if key(m, f, nil) == original {
			t.Fatalf("incompatible interpretations merged: %+v %+v", m, f)
		}
	}
	f := fact
	f.Subject = "ATLAS"
	f.Quote = "ATLAS uses Redis!"
	if key(base, f, nil) != original {
		t.Fatal("casing/punctuation not canonical")
	}
	later := base
	later.ValidFrom = now.Add(time.Hour)
	later.RecordedAt = later.ValidFrom
	if key(later, fact, nil) != original {
		t.Fatal("ordinary current observations cannot reconcile concurrently")
	}
	es := []knowledge.ExtractedEntity{{Name: "Atlas", Type: "project", Attributes: map[string]any{"owner": "Alice"}}}
	a := key(base, fact, es)
	es[0].Attributes["owner"] = "Bob"
	if key(base, fact, es) == a {
		t.Fatal("different extracted attributes discarded")
	}
}
