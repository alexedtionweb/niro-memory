package niromem_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func knowledgeBenchEngine(b *testing.B) (*niromem.Engine, []memory.Memory) {
	b.Helper()
	s := memstore.New(nil)
	e, err := niromem.Open(niromem.Options{Store: s})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.Close(); s.Close() })
	var facts []niromem.Fact
	for i := 0; i < 200; i++ {
		facts = append(facts, niromem.Fact{Scope: memory.Scope{Tenant: "bench", User: "project"},
			Text: fmt.Sprintf("Project database convention %d uses focused tests", i)})
	}
	changes, err := e.Remember(context.Background(), facts...)
	if err != nil {
		b.Fatal(err)
	}
	ms := make([]memory.Memory, len(changes))
	batch := &store.Batch{Tenant: "bench"}
	for i, ch := range changes {
		ms[i] = ch.Memory
		if i > 0 {
			batch.Relations = append(batch.Relations, relations.Link{ID: fmt.Sprint(i), Tenant: "bench",
				Source: ch.Memory.ID, Target: changes[i-1].Memory.ID, Relation: "supports",
				Confidence: 1, Origin: "validated", At: time.Now()})
		}
	}
	if _, err := s.Write(context.Background(), batch); err != nil {
		b.Fatal(err)
	}
	return e, ms
}

func BenchmarkKnowledgeStateLookup(b *testing.B) {
	e, _ := knowledgeBenchEngine(b)
	req := niromem.KnowledgeRequest{Input: knowledge.Input{Scope: memory.Scope{Tenant: "bench", User: "project"}, Text: "database focused tests"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.KnowledgeState(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkKnowledgeValidation(b *testing.B) {
	in := knowledge.Input{Scope: memory.Scope{Tenant: "bench", User: "project"}, OperationKey: "validation-bench", Text: "A new coding convention", Trust: memory.TrustHigh, Authority: true, Intent: "ADD"}
	state := knowledge.State{}
	plan, err := (knowledge.Deterministic{}).Plan(context.Background(), in, state)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := knowledge.Validate(in, plan, state, memory.Principal{User: "project"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkKnowledgeRelationshipTraversal(b *testing.B) {
	e, ms := knowledgeBenchEngine(b)
	req := niromem.RelationRequest{ListRequest: niromem.ListRequest{Tenant: "bench", User: "project", As: memory.Principal{User: "project"}},
		MemoryID: ms[100].ID, Depth: 2, MaxEdges: 100}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.KnowledgeRelations(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkKnowledgeSearch(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("relationships_%t", enabled), func(b *testing.B) {
			e, _ := knowledgeBenchEngine(b)
			p := search.Default["fast"]
			if enabled {
				p.RelationshipHops = 2
			}

			if err := e.PutSettings(context.Background(), "bench", niromem.Settings{Policies: map[string]search.Policy{"bench": p}}); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := e.Search(context.Background(), niromem.SearchRequest{Tenant: "bench", User: "project", Query: "database focused", Policy: "bench", Limit: 10}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkKnowledgeReconcileWithRelationships(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		s := memstore.New(nil)
		e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced"}})
		if err != nil {
			b.Fatal(err)
		}
		sc := memory.Scope{Tenant: "reconcile-bench", User: "project"}
		old, err := e.Remember(ctx, niromem.Fact{Scope: sc, Text: "Use PostgreSQL", Slot: "database"})
		if err != nil {
			b.Fatal(err)
		}
		_, err = e.CommitKnowledge(ctx, niromem.KnowledgeRequest{Input: knowledge.Input{
			Scope: sc, OperationKey: "correction", Text: "We already use SQLite", Trust: memory.TrustHigh,
			Authority: true, Intent: "CORRECT", TargetID: old[0].Memory.ID, ValidFrom: old[0].Memory.ValidFrom}})
		e.Close()
		s.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}
