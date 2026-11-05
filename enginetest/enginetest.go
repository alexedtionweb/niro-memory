// Package enginetest runs end-to-end engine scenarios against a store
// connector — the complement of storetest, which checks the contract in
// isolation. It exercises knowledge evolution, conversation processing by
// several stateless engines with concurrent workers, retrieval and forget.
//
//	func TestEngine(t *testing.T) {
//		enginetest.Run(t, func(t *testing.T) store.Store { return newEmptyStore(t) })
//	}
package enginetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/store"
)

// Factory returns a new, empty store.
type Factory func(t *testing.T) store.Store

// testRules is a small rules file used by the scenarios.
const testRules = `{"version":1,
 "entities":[{"type":"name","pattern":"\\b(?P<name>\\p{Lu}\\p{Ll}+(?: \\p{Lu}\\p{Ll}+)+)\\b"}],
 "facts":[
  {"id":"city","pattern":"\\b[Ii] (?:live|moved) (?:in|to) (?P<c>\\p{Lu}\\p{L}+)","emit":{"slot":"user.city","text":"User lives in {c}","entities":[{"type":"place","from":"{c}"}]}},
  {"id":"note","pattern":"^(?i:remember) (?P<x>.+)$","emit":{"text":"{x}"}}]}`

// Run executes the scenarios. Each gets a fresh store.
func Run(t *testing.T, newStore Factory) {
	t.Run("GroundedBackgroundEnrichment", func(t *testing.T) { testEnrichment(t, newStore) })
	t.Run("ConcurrentEnrichmentDeduplication", func(t *testing.T) { testEnrichmentDedup(t, newStore) })
	t.Run("EnrichmentDedupLifecycle", func(t *testing.T) { testEnrichmentDedupLifecycle(t, newStore) })
	t.Run("TypedEntityOnlyEnrichment", func(t *testing.T) { testEntityOnlyEnrichment(t, newStore) })
	t.Run("KnowledgeIntelligence", func(t *testing.T) { testKnowledge(t, newStore) })
	t.Run("DerivedRelationships", func(t *testing.T) { testRelationships(t, newStore) })
	rs, err := rules.Parse([]byte(testRules))
	if err != nil {
		t.Fatal(err)
	}
	open := func(t *testing.T, s store.Store) *niromem.Engine {
		e, err := niromem.Open(niromem.Options{Store: s, Rules: rs, Debounce: -1, Embedder: embed.Hash{Dims: 32}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.Close() })
		return e
	}
	ctx := context.Background()
	alice := memory.Scope{Tenant: "acme", User: "alice", Agent: "bot"}

	t.Run("KnowledgeEvolution", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		e := open(t, s)
		t0 := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Millisecond)
		ch, err := e.Remember(ctx, niromem.Fact{Scope: alice, Text: "Alice lives in Paris", Slot: "user.city", ValidFrom: t0})
		if err != nil {
			t.Fatal(err)
		}
		paris := ch[0].Memory
		ch, err = e.Remember(ctx, niromem.Fact{Scope: alice, Text: "Alice lives in Lisbon", Slot: "user.city", ValidFrom: t0.Add(24 * time.Hour)})
		if err != nil || ch[0].Action != memory.ActionSupersede {
			t.Fatalf("supersede: %+v %v", ch, err)
		}
		cur, _ := e.List(ctx, niromem.ListRequest{Tenant: "acme", User: "alice"})
		if len(cur) != 1 || cur[0].Text != "Alice lives in Lisbon" {
			t.Fatalf("current: %+v", cur)
		}
		past, _ := e.List(ctx, niromem.ListRequest{Tenant: "acme", User: "alice", ValidAt: t0.Add(time.Hour)})
		if len(past) != 1 || past[0].ID != paris.ID {
			t.Fatalf("as-of: %+v", past)
		}
		vs, _ := e.Versions(ctx, "acme", paris.ID)
		if len(vs) != 2 {
			t.Fatalf("versions: %d", len(vs))
		}
	})

	t.Run("StatelessWorkers", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		// Three engines on one store act as three stateless nodes.
		nodes := []*niromem.Engine{open(t, s), open(t, s), open(t, s)}
		var turns []niromem.Turn
		for i := 0; i < 30; i++ {
			turns = append(turns, niromem.Turn{ClientMsgID: fmt.Sprint(i), Role: "user", Text: fmt.Sprintf("Remember item %d is in box %d", i, i)})
		}
		run := alice
		run.Run = "r1"
		for i, n := range nodes { // each node appends a third, duplicates included
			if _, err := n.Append(ctx, run, turns[i*10:i*10+15-min(i*5, 5)], niromem.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for _, n := range nodes {
			wg.Add(1)
			go func(n *niromem.Engine) {
				defer wg.Done()
				w := n.Worker()
				w.Concurrency = 3
				for i := 0; i < 5; i++ {
					w.RunOnce(ctx, 10)
				}
			}(n)
		}
		wg.Wait()
		ms, _ := nodes[0].List(ctx, niromem.ListRequest{Tenant: "acme", User: "alice", Limit: 1000})
		if len(ms) != 30 {
			t.Fatalf("want 30 memories exactly once, got %d", len(ms))
		}
		for _, m := range ms {
			if len(m.Source.Evidence) != 1 {
				t.Fatalf("turn applied more than once: %+v", m)
			}
		}
		if have, total, _ := nodes[1].Coverage(ctx, "acme", "hash@1"); have != total {
			t.Fatalf("embedding coverage %d/%d", have, total)
		}
	})

	t.Run("RetrievalAndContext", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		e := open(t, s)
		run := alice
		run.Run = "chat"
		if _, err := e.Append(ctx, run, []niromem.Turn{{Role: "user", Text: "I moved to Kyoto. Remember the ryokan is booked for April"}}, niromem.AppendOptions{Wait: true}); err != nil {
			t.Fatal(err)
		}
		e.Remember(ctx, niromem.Fact{Scope: alice, Layer: memory.Profile, Text: "Vegetarian traveller"},
			niromem.Fact{Scope: memory.Scope{Tenant: "acme", User: "bob"}, Text: "Bob also booked a ryokan"})
		r, err := e.Search(ctx, niromem.SearchRequest{Tenant: "acme", User: "alice", Query: "ryokan april", Explain: true})
		if err != nil || len(r.Hits) == 0 || r.Hits[0].Memory.Text != "the ryokan is booked for April" {
			t.Fatalf("search: %+v %v", r, err)
		}
		for _, h := range r.Hits {
			if h.Memory.User != "alice" {
				t.Fatal("search leaked another user's memory")
			}
		}
		c, err := e.Context(ctx, niromem.ContextRequest{Tenant: "acme", User: "alice", Run: "chat", Query: "kyoto", TurnTokens: 100})
		if err != nil || len(c.Profile) != 2 || c.Window == nil || len(c.Window.Turns) != 1 {
			t.Fatalf("context: %+v %v", c, err)
		}
	})

	t.Run("Forget", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		e := open(t, s)
		run := alice
		run.Run = "r"
		e.Append(ctx, run, []niromem.Turn{{Role: "user", Text: "I live in Oslo"}}, niromem.AppendOptions{Wait: true})
		e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "acme", User: "bob"}, Text: "bob's note"})
		res, err := e.Forget(ctx, memory.Scope{Tenant: "acme", User: "alice"}, "test")
		if err != nil || res.Memories != 1 || res.Runs != 1 {
			t.Fatalf("%+v %v", res, err)
		}
		left, _ := e.List(ctx, niromem.ListRequest{Tenant: "acme", As: memory.Principal{Admin: true}, AllVersions: true, ShowExpired: true})
		if len(left) != 1 || left[0].User != "bob" {
			t.Fatalf("%+v", left)
		}
		if eps, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "acme", User: "alice", IncludeDeleted: true}); len(eps) != 0 {
			t.Fatal("episodes survived forget")
		}
	})
}
