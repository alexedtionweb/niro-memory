package search_test

import (
	"context"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	"github.com/alexedtionweb/niro-memory/vector"
)

type deadlineEmbedder struct {
	embed.Hash
	deadlines chan time.Duration
}

func (e deadlineEmbedder) Embed(ctx context.Context, purpose embed.Purpose, in []embed.Input) ([][]float32, embed.Usage, error) {
	deadline, _ := ctx.Deadline()
	e.deadlines <- time.Until(deadline)
	select {
	case <-time.After(120 * time.Millisecond):
		return e.Hash.Embed(ctx, purpose, in)
	case <-ctx.Done():
		return nil, embed.Usage{}, ctx.Err()
	}
}

func TestLocalModelBudgetsReturnVectorOnlyMatches(t *testing.T) {
	for _, name := range []string{"default", "deep"} {
		t.Run(name, func(t *testing.T) {
			s := memstore.New(nil)
			e := deadlineEmbedder{Hash: embed.Hash{Dims: 64}, deadlines: make(chan time.Duration, 1)}
			ms := seed(t, s, m("u", "A stored assertion with unrelated wording"))
			values, _, err := e.Hash.Embed(ctx, embed.Query, []embed.Input{{Text: "semantic recall"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Write(ctx, &store.Batch{Tenant: "t", Vectors: []vector.Point{{
				Model: e.Model().ID, Target: vector.Target{Kind: vector.TargetMemory, ID: ms[0].ID}, User: "u", Values: values[0],
			}}})
			if err != nil {
				t.Fatal(err)
			}
			policy := search.Default[name]
			if policy.EmbedBudget < 30*time.Second {
				t.Fatalf("local model budget remains too short: %s", policy.EmbedBudget)
			}
			sr := search.Searcher{Store: memstoreNoLexical{s}, Index: s, Embedder: e}
			result, err := sr.Search(ctx, search.Request{Tenant: "t", User: "u", Text: "semantic recall", Model: e.Model().ID, Policy: policy})
			if err != nil || len(result.Hits) != 1 || result.Hits[0].Memory.ID != ms[0].ID || !strategies(result)["vector"].Ran {
				t.Fatalf("slow query lost vector-only match: %+v %v", result, err)
			}
			if got := <-e.deadlines; got < policy.EmbedBudget-time.Second {
				t.Fatalf("hidden generation deadline %s shorter than policy %s", got, policy.EmbedBudget)
			}
		})
	}
	for _, name := range []string{"fast", "voice"} {
		if search.Default[name].EmbedBudget != 0 {
			t.Fatalf("%s no longer preserves no-wait behavior", name)
		}
	}
}
