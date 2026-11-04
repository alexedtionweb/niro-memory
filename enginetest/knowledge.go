package enginetest

import (
	"context"
	"errors"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

func testKnowledge(t *testing.T, factory Factory) {
	t.Run("GraphPrivacyCyclesAndDependencyRecovery", func(t *testing.T) { testKnowledgeGraphAndJobs(t, factory) })
	t.Run("LayeredConsolidation", func(t *testing.T) { testKnowledgeConsolidation(t, factory) })
	t.Run("TemporalPerspectives", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
		now := base
		e, err := niromem.Open(niromem.Options{Store: s, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		ctx := context.Background()
		sc := memory.Scope{Tenant: "temporal-perspectives", User: "alice"}
		old, err := e.Remember(ctx, niromem.Fact{Scope: sc, Text: "Alice lives in Lisbon", Slot: "city", ValidFrom: base})
		if err != nil {
			t.Fatal(err)
		}
		now = base.Add(5 * 24 * time.Hour)
		changeAt := base.Add(3 * 24 * time.Hour)
		next, err := e.Remember(ctx, niromem.Fact{Scope: sc, Text: "Alice lives in Porto", Slot: "city", ValidFrom: changeAt})
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name         string
			valid, known time.Time
			want         string
		}{
			{"believed_before_learning_the_move", base.Add(4 * 24 * time.Hour), base.Add(2 * 24 * time.Hour), old[0].Memory.ID},
			{"currently_believed_about_before_the_move", base.Add(24 * time.Hour), now, old[0].Memory.ID},
			{"currently_believed_about_after_the_move", base.Add(4 * 24 * time.Hour), now, next[0].Memory.ID},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				ms, err := e.List(ctx, niromem.ListRequest{Tenant: sc.Tenant, User: sc.User, ValidAt: tt.valid, KnownAt: tt.known})
				if err != nil || len(ms) != 1 || ms[0].ID != tt.want {
					t.Fatalf("historical query: %+v %v; want %s", ms, err, tt.want)
				}
			})
		}
		now = base.Add(7 * 24 * time.Hour)
		fixText := "Alice has lived in Braga since the move"
		corrected, err := e.Update(ctx, sc.Tenant, next[0].Memory.ID, niromem.Patch{Text: &fixText, OperationKey: "correct-braga"})
		if err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct {
			known time.Time
			want  string
		}{
			{base.Add(6 * 24 * time.Hour), next[0].Memory.ID},
			{now, corrected.ID},
		} {
			ms, err := e.List(ctx, niromem.ListRequest{Tenant: sc.Tenant, User: sc.User, ValidAt: base.Add(4 * 24 * time.Hour), KnownAt: tt.known})
			if err != nil || len(ms) != 1 || ms[0].ID != tt.want {
				t.Fatalf("correction history: %+v %v", ms, err)
			}
		}
		original, err := e.Get(ctx, sc.Tenant, memory.Principal{Admin: true}, next[0].Memory.ID)
		if err != nil || original.Text != "Alice lives in Porto" {
			t.Fatalf("original assertion changed: %+v %v", original, err)
		}
	})
	t.Run("IdempotentExplicitWrites", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		if _, ok := s.(store.Operations); !ok {
			t.Skip("optional durable operation ledger unavailable")
		}
		ctx := context.Background()
		e, err := niromem.Open(niromem.Options{Store: s})
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		f := niromem.Fact{Scope: memory.Scope{Tenant: "idempotent", User: "project"}, Text: "Use focused tests", OperationKey: "write-1"}
		first, err := e.Remember(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		restarted, err := niromem.Open(niromem.Options{Store: s})
		if err != nil {
			t.Fatal(err)
		}
		defer restarted.Close()
		second, err := restarted.Remember(ctx, f)
		if err != nil || len(second) != 1 || second[0].Memory.ID != first[0].Memory.ID {
			t.Fatalf("replayed write: %+v %v", second, err)
		}
		h, err := e.History(ctx, f.Tenant, first[0].Memory.ID)
		if err != nil || len(h) != 1 {
			t.Fatalf("duplicate transition: %+v %v", h, err)
		}
		f.Text = "Changed payload"
		if _, err := e.Remember(ctx, f); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("key reuse: %v", err)
		}
	})
	t.Run("ValidatedKnowledgeTransitions", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		if _, ok := s.(store.Operations); !ok {
			t.Skip("operation ledger unavailable")
		}
		if _, ok := s.(relations.Store); !ok {
			t.Skip("relationships unavailable")
		}
		ctx := context.Background()
		e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced"}})
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		sc := memory.Scope{Tenant: "knowledge", User: "alice"}
		req := niromem.KnowledgeRequest{As: memory.Principal{User: "alice"}, Input: knowledge.Input{
			Scope: sc, OperationKey: "decision-1", Text: "Use PostgreSQL in production", Trust: memory.TrustHigh, Authority: true,
			Assertion: &memory.Assertion{Subject: "production", Predicate: "database", Object: "PostgreSQL", EpistemicType: "decision", Confidence: 1},
			Intent:    "ADD"}}
		preview, err := e.EvaluateKnowledge(ctx, req)
		if err != nil || preview.Status != "proposed" {
			t.Fatalf("preview: %+v %v", preview, err)
		}
		before, err := e.List(ctx, niromem.ListRequest{Tenant: sc.Tenant, User: sc.User})
		if err != nil || len(before) != 0 {
			t.Fatalf("dry run wrote knowledge: %+v %v", before, err)
		}
		committed, err := e.CommitKnowledge(ctx, req)
		if err != nil || len(committed.Changes) != 1 {
			t.Fatalf("commit: %+v %v", committed, err)
		}
		retry, err := e.CommitKnowledge(ctx, req)
		if err != nil || !retry.Replayed || retry.Changes[0].Memory.ID != committed.Changes[0].Memory.ID {
			t.Fatalf("replay: %+v %v", retry, err)
		}
		req.Input.OperationKey = "correct-2"
		req.Input.TargetID = committed.Changes[0].Memory.ID
		req.Input.Intent = "CORRECT"
		req.Input.Text = "Production already uses SQLite"
		req.Input.Assertion.Object = "SQLite"
		independent, err := e.Remember(ctx, niromem.Fact{Scope: sc, Text: req.Input.Text,
			Assertion: req.Input.Assertion})
		if err != nil {
			t.Fatal(err)
		}
		correction, err := e.CommitKnowledge(ctx, req)
		if err != nil || len(correction.Changes) != 1 {
			t.Fatalf("correct: %+v %v", correction, err)
		}
		if correction.Changes[0].Memory.ID == independent[0].Memory.ID {
			t.Fatal("correction deduplicated away its explicit version lineage")
		}
		versions, err := e.Versions(ctx, sc.Tenant, committed.Changes[0].Memory.ID)
		if err != nil || len(versions) != 2 {
			t.Fatalf("lineage: %+v %v", versions, err)
		}
		if versions[0].Text == versions[1].Text {
			t.Fatal("original assertion not preserved")
		}
		trace, err := e.KnowledgeTrace(ctx, sc.Tenant, memory.Principal{User: sc.User}, "correct-2")
		if err != nil || trace.Status != "committed" {
			t.Fatalf("trace: %+v %v", trace, err)
		}
		if _, err := e.KnowledgeTrace(ctx, sc.Tenant, memory.Principal{User: "other"}, "correct-2"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("private trace leak: %v", err)
		}
	})
}
