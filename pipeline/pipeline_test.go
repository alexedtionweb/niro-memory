package pipeline_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

var ctx = context.Background()

func setup(t *testing.T) (*memstore.Store, *pipeline.Reconciler) {
	s := memstore.New(nil)
	return s, &pipeline.Reconciler{Store: s, Now: time.Now, Limits: memory.DefaultLimits}
}

func apply(t *testing.T, s store.Store, r *pipeline.Reconciler, props ...pipeline.Proposal) []pipeline.Change {
	t.Helper()
	b := &store.Batch{Tenant: "t"}
	ch, err := r.Plan(ctx, "t", props, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, b); err != nil {
		t.Fatal(err)
	}
	return ch
}

func fact(text, slot string, at time.Time) pipeline.Proposal {
	return pipeline.Proposal{Memory: memory.Memory{Scope: memory.Scope{User: "alice"}, Text: text, Slot: slot, ValidFrom: at,
		Source: memory.Source{Evidence: []memory.Evidence{{Episode: "e"}}}}}
}

func current(t *testing.T, s store.Store) []memory.Memory {
	t.Helper()
	ms, err := s.Find(ctx, store.Query{Tenant: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestReconcileLifecycle(t *testing.T) {
	s, r := setup(t)
	t0 := time.Now().Add(-48 * time.Hour)
	add := apply(t, s, r, fact("Alice lives in Paris", "user.city", t0))
	if add[0].Action != memory.ActionAdd {
		t.Fatalf("%+v", add)
	}
	paris := add[0].Memory

	dup := apply(t, s, r, fact("alice lives in  PARIS", "user.city", t0.Add(time.Hour)))
	if dup[0].Action != memory.ActionEvidence || dup[0].Memory.ID != paris.ID {
		t.Fatalf("duplicate: %+v", dup)
	}

	t1 := t0.Add(24 * time.Hour)
	sup := apply(t, s, r, fact("Alice lives in Lisbon", "user.city", t1))
	if sup[0].Action != memory.ActionSupersede || sup[0].Previous != paris.ID || sup[0].Memory.Version != 2 {
		t.Fatalf("supersede: %+v", sup)
	}
	cur := current(t, s)
	if len(cur) != 1 || cur[0].Text != "Alice lives in Lisbon" {
		t.Fatalf("current: %+v", cur)
	}
	// As of t0+1h Alice lived in Paris: history is preserved bi-temporally.
	past, _ := s.Find(ctx, store.Query{Tenant: "t", ValidAt: t0.Add(time.Hour)})
	if len(past) != 1 || past[0].ID != paris.ID || past[0].Heat != 1 {
		t.Fatalf("as-of: %+v", past)
	}
	// A fact that already ended is stored as history and leaves the current value alone.
	berlin := fact("Alice lived in Berlin", "user.city", t0.Add(-48*time.Hour))
	end := t0.Add(-24 * time.Hour)
	berlin.Memory.ValidTo = &end
	if old := apply(t, s, r, berlin); old[0].Action != memory.ActionAdd {
		t.Fatalf("history: %+v", old)
	}
	if cur := current(t, s); len(cur) != 1 || cur[0].Text != "Alice lives in Lisbon" {
		t.Fatal("history replaced current value")
	}
	// Invalidate with a non-matching value does nothing; matching ends validity.
	inv := pipeline.Proposal{Memory: memory.Memory{Scope: memory.Scope{User: "alice"}, Slot: "user.city", ValidFrom: t1.Add(time.Hour)}, Invalidate: true, Match: "Paris"}
	if ch := apply(t, s, r, inv); len(ch) != 1 || ch[0].Action != pipeline.ActionNoop {
		t.Fatalf("invalidate mismatch: %+v", ch)
	}
	inv.Match = "lisbon"
	if ch := apply(t, s, r, inv); len(ch) != 1 || ch[0].Action != memory.ActionInvalidate {
		t.Fatalf("invalidate: %+v", ch)
	}
	if cur := current(t, s); len(cur) != 0 {
		t.Fatalf("after invalidate: %+v", cur)
	}
	h, _ := s.History(ctx, "t", paris.ID)
	if len(h) != 3 || h[0].Action != memory.ActionAdd || h[1].Action != memory.ActionEvidence || h[2].Action != memory.ActionSupersede {
		t.Fatalf("history: %+v", h)
	}
}

func TestReconcileInBatch(t *testing.T) {
	s, r := setup(t)
	now := time.Now().Add(-time.Hour)
	ch := apply(t, s, r,
		fact("Alice lives in Rome", "user.city", now),
		fact("alice lives in rome", "user.city", now),
		fact("Alice lives in Oslo", "user.city", now.Add(time.Minute)),
		pipeline.Proposal{Memory: memory.Memory{Text: "  "}},
	)
	if ch[1].Action != memory.ActionEvidence || ch[2].Action != memory.ActionSupersede || ch[3].Action != pipeline.ActionReject {
		t.Fatalf("%+v", ch)
	}
	cur := current(t, s)
	if len(cur) != 1 || cur[0].Text != "Alice lives in Oslo" || cur[0].Supersedes != ch[0].Memory.ID {
		t.Fatalf("%+v", cur)
	}
	// Same valid time: the second value is a correction of the first.
	ch = apply(t, s, r, fact("Alice's car is red", "user.car", now), fact("Alice's car is blue", "user.car", now))
	all, _ := s.Get(ctx, "t", []string{ch[0].Memory.ID})
	if all[0].InvalidatedAt == nil {
		t.Fatalf("correction must invalidate: %+v", all[0])
	}
}

func rulesSet(t *testing.T) *rules.Set {
	b, err := os.ReadFile("../configs/rules.example.json")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func processor(s store.Store, r *pipeline.Reconciler, rs *rules.Set) *pipeline.Processor {
	return &pipeline.Processor{Store: s, Reconciler: r, BatchSize: 2,
		RulesFor: func(context.Context, string) (*rules.Set, error) { return rs, nil }}
}

func turn(id, role, text string, at time.Time) memory.Episode {
	return memory.Episode{Scope: memory.Scope{User: "alice", Agent: "bot", Run: "r1"}, ClientMsgID: id, Role: role, OccurredAt: at,
		Parts: []memory.Part{{Kind: memory.PartText, Text: text}}}
}

func TestProcessRun(t *testing.T) {
	s, r := setup(t)
	rs := rulesSet(t)
	p := processor(s, r, rs)
	t0 := time.Now().Add(-time.Hour)
	_, err := s.Write(ctx, &store.Batch{Tenant: "t", Episodes: []memory.Episode{
		turn("1", "user", "Hi! My name is Alice. I live in Paris.", t0),
		turn("2", "assistant", "Nice to meet you. My name is Bot.", t0.Add(time.Second)),
		turn("3", "user", "Actually I moved to Lisbon last week.", t0.Add(2*time.Second)),
		{Scope: memory.Scope{User: "alice", Run: "r1"}, Role: "tool", OccurredAt: t0, Parts: []memory.Part{{Kind: memory.PartToolResult, Name: "get_order", Data: []byte(`{"order":{"id":"ORD-77","status":"shipped"}}`)}}},
		turn("5", "user", "ok", t0.Add(3*time.Second)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.ProcessRun(ctx, "t", "r1")
	if err != nil || st.Episodes != 5 {
		t.Fatalf("%+v %v", st, err)
	}
	cur := current(t, s)
	texts := map[string]memory.Memory{}
	for _, m := range cur {
		texts[m.Text] = m
	}
	if len(cur) != 3 {
		t.Fatalf("want 3 current memories, got %d: %v", len(cur), texts)
	}
	lisbon, ok := texts["User lives in Lisbon"]
	if !ok || lisbon.Version != 2 || lisbon.Source.Kind != "rules" || lisbon.Source.Evidence[0].Episode == "" || lisbon.Run != "r1" {
		t.Fatalf("lisbon: %+v", lisbon)
	}
	if !lisbon.ValidFrom.Before(t0) {
		t.Fatal("'last week' must move valid_from before the turn")
	}
	if o := texts["Order ORD-77 status is shipped"]; o.Source.Trust != memory.TrustLow {
		t.Fatalf("tool output must be low trust: %+v", o.Source)
	}
	run, _ := s.Run(ctx, "t", "r1")
	if run.ProcessedSeq != 5 {
		t.Fatalf("cursor %d", run.ProcessedSeq)
	}
	// Idempotent: nothing new to process.
	if st, _ := p.ProcessRun(ctx, "t", "r1"); st.Episodes != 0 {
		t.Fatal("reprocessed episodes")
	}
}

func TestConcurrentProcessorsNeverDuplicate(t *testing.T) {
	s, r := setup(t)
	rs := rulesSet(t)
	var eps []memory.Episode
	t0 := time.Now().Add(-time.Hour)
	for i := 0; i < 40; i++ {
		eps = append(eps, turn(string(rune('a'+i)), "user", "Remember that item "+string(rune('A'+i%26))+string(rune('a'+i/26))+" is important", t0.Add(time.Duration(i)*time.Second)))
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Episodes: eps}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := processor(s, r, rs).ProcessRun(ctx, "t", "r1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	cur := current(t, s)
	if len(cur) != 40 {
		t.Fatalf("want 40 memories, got %d", len(cur))
	}
	for _, m := range cur {
		if m.Heat != 0 || len(m.Source.Evidence) != 1 {
			t.Fatalf("episode applied twice: %+v", m)
		}
	}
}

func TestRederive(t *testing.T) {
	s, r := setup(t)
	p := processor(s, r, rulesSet(t))
	t0 := time.Now().Add(-time.Hour)
	rec, _ := s.Write(ctx, &store.Batch{Tenant: "t", Episodes: []memory.Episode{turn("1", "user", "I live in Paris.", t0), turn("2", "user", "I'm allergic to peanuts.", t0)}})
	if _, err := p.ProcessRun(ctx, "t", "r1"); err != nil {
		t.Fatal(err)
	}
	e1, e2 := rec.Episodes[0].Episode.ID, rec.Episodes[1].Episode.ID
	_, _ = s.Write(ctx, &store.Batch{Tenant: "t", EpisodeEdits: []store.EpisodeEdit{{ID: e1, Status: memory.Edited, Parts: []memory.Part{{Kind: memory.PartText, Text: "I live in Rome."}}}}})
	ch, err := p.Rederive(ctx, "t", e1)
	if err != nil || len(ch) != 2 || ch[0].Action != memory.ActionInvalidate || ch[1].Memory.Text != "User lives in Rome" {
		t.Fatalf("edit: %+v %v", ch, err)
	}
	_, _ = s.Write(ctx, &store.Batch{Tenant: "t", EpisodeEdits: []store.EpisodeEdit{{ID: e2, Status: memory.Deleted}}})
	if ch, _ := p.Rederive(ctx, "t", e2); len(ch) != 1 || ch[0].Action != memory.ActionInvalidate {
		t.Fatalf("delete: %+v", ch)
	}
	cur := current(t, s)
	if len(cur) != 1 || cur[0].Text != "User lives in Rome" {
		t.Fatalf("%+v", cur)
	}
}
