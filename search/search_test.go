package search_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/cache"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	"github.com/alexedtionweb/niro-memory/vector"
)

var ctx = context.Background()

func seed(t *testing.T, s store.Store, ms ...memory.Memory) []memory.Memory {
	t.Helper()
	now := time.Now().Add(-time.Hour)
	for i := range ms {
		m := &ms[i]
		m.ID, m.Tenant, m.Hash = memory.NewID(), "t", memory.Hash(m.Text)
		if m.Kind == "" {
			m.Kind = memory.Semantic
		}
		m.Layer, m.ValidFrom, m.RecordedAt, m.Version = memory.Atom, now, now, 1
		if m.Visibility == "" {
			m.Visibility = memory.Team
		}
		m.Confidence, m.Importance = 0.8, 0.5
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Memories: ms}); err != nil {
		t.Fatal(err)
	}
	return ms
}

func m(user, text string, ents ...string) memory.Memory {
	x := memory.Memory{Scope: memory.Scope{User: user}, Text: text}
	for _, e := range ents {
		x.Entities = append(x.Entities, memory.EntityRef{Name: e})
	}
	return x
}

func strategies(r *search.Result) map[string]search.Step {
	out := map[string]search.Step{}
	for _, s := range r.Trace {
		out[s.Strategy] = s
	}
	return out
}

func TestPlannerPaths(t *testing.T) {
	s := memstore.New(nil)
	ms := seed(t, s,
		m("alice", "Alice prefers green tea in the morning", "Alice"),
		m("alice", "Alice works at Acme Corp", "Alice", "Acme Corp"),
		m("bob", "Bob drinks coffee", "Bob"),
		m("alice", "The Acme office is in Lisbon", "Acme Corp", "Lisbon"),
	)
	sr := &search.Searcher{Store: s}

	r, err := sr.Search(ctx, search.Request{Tenant: "t", IDs: []string{ms[0].ID, ms[2].ID}, User: "alice"})
	if err != nil || len(r.Hits) != 1 || r.Trace[0].Strategy != "exact" {
		t.Fatalf("exact with scope: %+v %v", r, err)
	}
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "alice", Limit: 2})
	if len(r.Hits) != 2 || r.Trace[0].Strategy != "filter" || r.Hits[0].Memory.ID != ms[3].ID {
		t.Fatalf("filter: %+v", r)
	}
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "alice", Text: "what tea does she like?", Explain: true})
	st := strategies(r)
	if !st["lexical"].Ran || !st["entity"].Ran || st["vector"].Ran || len(r.Hits) == 0 || r.Hits[0].Memory.ID != ms[0].ID {
		t.Fatalf("hybrid: %+v", r)
	}
	if e := r.Hits[0].Explain; e == nil || e.Lexical <= 0 || e.Final != r.Hits[0].Score || e.Final > 1 {
		t.Fatalf("explain: %+v", r.Hits[0].Explain)
	}
	if !strings.Contains(st["vector"].Reason, "no vector index") {
		t.Fatalf("vector skip reason: %q", st["vector"].Reason)
	}
	// Entity-only recall: no shared keyword with "Acme Corp" memories except the entity.
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "alice", Text: "acme corp", Explain: true})
	if len(r.Hits) != 2 {
		t.Fatalf("entity: %+v", r.Hits)
	}
	for _, h := range r.Hits {
		if h.Explain.Entity <= 0 {
			t.Fatalf("entity signal missing: %+v", h.Explain)
		}
	}
	// Bob's memory never leaks into Alice's results.
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "alice", Text: "coffee"})
	if len(r.Hits) != 0 {
		t.Fatalf("scope leak: %+v", r.Hits)
	}
	// Threshold.
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "alice", Text: "tea", Threshold: 0.99})
	if len(r.Hits) != 0 {
		t.Fatal("threshold")
	}
	// Other tenant sees nothing.
	if r, _ := sr.Search(ctx, search.Request{Tenant: "x", Text: "tea"}); len(r.Hits) != 0 {
		t.Fatal("tenant leak")
	}
}

func TestGraphHops(t *testing.T) {
	s := memstore.New(nil)
	ms := seed(t, s,
		m("u", "Project Falcon is led by Maria", "Falcon", "Maria"),
		m("u", "Maria lives in Porto", "Maria", "Porto"),
	)
	sr := &search.Searcher{Store: s}
	p := search.Default["deep"]
	r, _ := sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "falcon", Policy: p, Explain: true})
	if len(r.Hits) != 2 {
		t.Fatalf("two hops should reach Maria's city: %+v", r.Hits)
	}
	for _, h := range r.Hits {
		if h.Memory.ID == ms[1].ID && h.Explain.Entity >= 1 {
			t.Fatal("second hop must score lower")
		}
	}
}

type slowEmbedder struct {
	embed.Hash
	delay time.Duration
}

func (s slowEmbedder) Embed(ctx context.Context, p embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	time.Sleep(s.delay)
	return s.Hash.Embed(ctx, p, in)
}

func TestVectorAndBudget(t *testing.T) {
	s := memstore.New(nil)
	emb := embed.Hash{Dims: 64}
	ms := seed(t, s,
		m("u", "the cat sat on the mat"),
		m("u", "quantum chromodynamics lecture notes"),
		m("other", "the cat sat on the mat too"),
	)
	var pts []vector.Point
	for _, x := range ms {
		v, _, _ := emb.Embed(ctx, embed.Document, []embed.Input{{Text: x.Text}})
		pts = append(pts, vector.Point{Model: emb.Model().ID, Target: vector.Target{Kind: vector.TargetMemory, ID: x.ID}, User: x.User, Values: v[0]})
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Vectors: pts}); err != nil {
		t.Fatal(err)
	}
	c := cache.NewLRU(100)
	sr := &search.Searcher{Store: memstoreNoLexical{s}, Index: s, Embedder: emb, Cache: c}
	r, err := sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "cat on a mat", Model: emb.Model().ID, Explain: true})
	if err != nil || len(r.Hits) == 0 || r.Hits[0].Memory.ID != ms[0].ID || !strategies(r)["vector"].Ran {
		t.Fatalf("vector: %+v %v", r, err)
	}
	for _, h := range r.Hits {
		if h.Memory.User != "u" {
			t.Fatal("vector hit escaped scope")
		}
	}
	// Caller-provided vector, no embedder needed.
	v, _, _ := emb.Embed(ctx, embed.Query, []embed.Input{{Text: "quantum lecture"}})
	r, _ = (&search.Searcher{Store: s, Index: s}).Search(ctx, search.Request{Tenant: "t", User: "u",
		Vector: &search.QueryVector{Model: emb.Model().ID, Values: v[0]}})
	if len(r.Hits) == 0 || r.Hits[0].Memory.ID != ms[1].ID {
		t.Fatalf("byo vector: %+v", r.Hits)
	}
	// A slow embedder is skipped within budget, and warms the cache in the background.
	slow := slowEmbedder{Hash: emb, delay: 120 * time.Millisecond}
	sr = &search.Searcher{Store: s, Index: s, Embedder: slow, Cache: c}
	bounded := search.Default["default"]
	bounded.EmbedBudget = 50 * time.Millisecond
	start := time.Now()
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "slow query about cats", Model: emb.Model().ID, Policy: bounded})
	if time.Since(start) > 100*time.Millisecond || strategies(r)["vector"].Ran || !strings.Contains(strategies(r)["vector"].Reason, "budget") {
		t.Fatalf("budget not enforced: %v %+v", time.Since(start), strategies(r)["vector"])
	}
	time.Sleep(200 * time.Millisecond)
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "slow query about cats", Model: emb.Model().ID})
	if st := strategies(r)["vector"]; !st.Ran || !strings.Contains(st.Reason, "cached") {
		t.Fatalf("cache warm-up: %+v", st)
	}
	// Model mismatch: never compare across embedding spaces.
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "cats", Model: "other@1"})
	if strategies(r)["vector"].Ran {
		t.Fatal("embedded with a non-active model")
	}
}

// memstoreNoLexical hides full-text search to isolate the vector strategy.
type memstoreNoLexical struct{ store.Store }

func TestRRFAndMMR(t *testing.T) {
	s := memstore.New(nil)
	seed(t, s,
		m("u", "paris travel tips for spring"),
		m("u", "paris travel tips for spring!"),
		m("u", "paris museums open late on friday"),
	)
	sr := &search.Searcher{Store: s}
	p := search.Default["default"]
	p.Fusion = "rrf"
	r, err := sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "paris travel", Policy: p, Explain: true})
	if err != nil || len(r.Hits) != 3 || r.Hits[0].Explain.Relevance > 1 {
		t.Fatalf("rrf: %+v %v", r, err)
	}
	p = search.Default["default"]
	p.MMRLambda = 0.3
	r, _ = sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "paris travel spring", Policy: p, Limit: 2})
	if len(r.Hits) != 2 || !strings.Contains(r.Hits[1].Memory.Text, "museums") {
		t.Fatalf("mmr should prefer the diverse result: %+v", r.Hits)
	}
}

func TestVisibility(t *testing.T) {
	s := memstore.New(nil)
	secret := m("alice", "alice secret diary entry")
	secret.Visibility = memory.Private
	shared := m("alice", "alice diary shared with support")
	shared.Visibility, shared.Grants = memory.Restricted, []string{"role:support"}
	seed(t, s, secret, shared)
	sr := &search.Searcher{Store: s}
	for _, c := range []struct {
		as   memory.Principal
		want int
	}{
		{memory.Principal{User: "alice"}, 2},
		{memory.Principal{User: "eve"}, 0},
		{memory.Principal{User: "sam", Roles: []string{"support"}}, 1},
		{memory.Principal{Admin: true}, 2},
	} {
		r, _ := sr.Search(ctx, search.Request{Tenant: "t", As: c.as, Text: "diary"})
		if len(r.Hits) != c.want {
			t.Errorf("%+v: got %d want %d", c.as, len(r.Hits), c.want)
		}
	}
}
