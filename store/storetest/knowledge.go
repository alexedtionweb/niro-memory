package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
)

func testOperations(t *testing.T, s store.Store) {
	ops, ok := s.(store.Operations)
	if !ok {
		t.Skip("no operation receipts")
	}
	if _, err := ops.Operation(ctx, "t", "absent"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("missing receipt: %v", err)
	}
	m := mem("t", "once")
	b := &store.Batch{Tenant: "t", OperationKey: "k", OperationHash: "h", Result: json.RawMessage("{\n \"id\": \"original\"\n}"), Memories: []memory.Memory{m}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	original, replay := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Write(ctx, b)
			if err != nil {
				t.Errorf("concurrent replay: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Replayed {
				replay++
			} else {
				original++
			}
			if string(r.Result) != string(b.Result) || r.OperationHash != "h" {
				t.Errorf("receipt: %+v", r)
			}
		}()
	}
	wg.Wait()
	if original != 1 || replay != 7 {
		t.Fatalf("original=%d replay=%d", original, replay)
	}
	r, err := ops.Operation(ctx, "t", "k")
	if err != nil || r.Replayed || string(r.Result) != string(b.Result) {
		t.Fatalf("stored receipt: %+v %v", r, err)
	}
	if _, err := ops.Operation(ctx, "other", "k"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("tenant receipt leak: %v", err)
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", OperationKey: "k", OperationHash: "different"}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("hash conflict: %v", err)
	}
	// Replay precedes even stale preconditions and returns the original result.
	r = write(t, s, &store.Batch{Tenant: "t", OperationKey: "k", OperationHash: "h", Checks: []store.Check{{ID: "missing"}}, Result: json.RawMessage(`null`)})
	if !r.Replayed || string(r.Result) != string(b.Result) {
		t.Fatalf("bad replay: %+v", r)
	}
	empty := &store.Batch{Tenant: "t", OperationKey: "empty", OperationHash: "empty", Result: json.RawMessage(`[]`)}
	if empty.Empty() {
		t.Fatal("receipt-only batch is not empty")
	}
	write(t, s, empty)
	if _, err := ops.Operation(ctx, "t", "empty"); err != nil {
		t.Fatal(err)
	}
}

func testRelationHistory(t *testing.T, s store.Store) {
	rs, ok := s.(relations.Store)
	if !ok {
		t.Skip("no relations")
	}
	source, target := mem("t", "derived"), mem("t", "basis")
	foreign := mem("other", "foreign")
	write(t, s, &store.Batch{Tenant: "other", Memories: []memory.Memory{foreign}})
	r := relations.Link{ID: "link", Source: source.ID, Target: target.ID, Relation: "derived_from", Evidence: []memory.Evidence{{}}, At: base, ValidFrom: base}
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{source, target}, Relations: []relations.Link{r}})
	read := func() []relations.Link {
		t.Helper()
		got, err := rs.RelationshipSets(ctx, "t", []string{target.ID, source.ID})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); len(got) != 1 || got[0].Tenant != "t" {
		t.Fatalf("links: %+v", got)
	}
	bad := r
	bad.ID = "foreign"
	bad.Target = foreign.ID
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Relations: []relations.Link{bad}, Settings: []byte("must rollback")}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("cross-tenant link: %v", err)
	}
	if settings, _ := s.Settings(ctx, "t"); len(settings) != 0 {
		t.Fatal("partial settings mutation")
	}
	bad = r
	bad.Relation = "contradicts"
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Relations: []relations.Link{bad}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("changed link meaning: %v", err)
	}
	at := base.Add(time.Hour)
	// A failing late cursor must roll back closure, receipt and notifications.
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", OperationKey: "failed", OperationHash: "h", Closes: []store.Close{{ID: target.ID, ValidTo: &at}}, Cursors: []store.Cursor{{Run: "missing", From: 1}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("rollback: %v", err)
	}
	if got := read(); len(got) != 1 || got[0].InvalidatedAt != nil {
		t.Fatalf("failed close changed link: %+v", got)
	}
	if jobs, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: "knowledge.reevaluate"}); len(jobs) != 0 {
		t.Fatal("failed transaction enqueued jobs")
	}
	if ops, ok := s.(store.Operations); ok {
		if _, err := ops.Operation(ctx, "t", "failed"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("failed receipt: %v", err)
		}
	}
	write(t, s, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: target.ID, ValidTo: &at}}})
	got := read()
	if len(got) != 1 || got[0].InvalidatedAt == nil || got[0].Relation != r.Relation || len(got[0].Evidence) != 1 {
		t.Fatalf("history lost: %+v", got)
	}
	if ms, _ := s.Get(ctx, "t", []string{target.ID}); len(ms) != 1 {
		t.Fatal("close deleted historical memory")
	}
	jobs, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", Kind: "knowledge.reevaluate"})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("dependency jobs: %+v %v", jobs, err)
	}
	var payload map[string]string
	if err := json.Unmarshal(jobs[0].Payload, &payload); err != nil || payload["id"] != source.ID || payload["source"] != target.ID || jobs[0].Key == "" {
		t.Fatalf("dependency payload: %s", jobs[0].Payload)
	}
	write(t, s, &store.Batch{Tenant: "t", Deletes: []string{target.ID}})
	if got := read(); len(got) != 0 {
		t.Fatalf("privacy deletion retained links: %+v", got)
	}
}

func testKnowledgeChecks(t *testing.T, s store.Store) {
	m := mem("t", "checked")
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{m}})
	for _, c := range []store.Check{{ID: m.ID, Version: 2}, {ID: "absent", Version: 1}} {
		_, err := s.Write(ctx, &store.Batch{Tenant: "t", Checks: []store.Check{c}, Settings: []byte("bad")})
		if !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("stale check: %v", err)
		}
	}
	write(t, s, &store.Batch{Tenant: "t", Checks: []store.Check{{ID: m.ID, Version: 1, Open: true}}})
	at := base.Add(time.Hour)
	write(t, s, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: m.ID, ValidTo: &at}}})
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Checks: []store.Check{{ID: m.ID, Version: 1, Open: true}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("closed check: %v", err)
	}
}

func testKnownBeforeClosure(t *testing.T, s store.Store) {
	m := mem("t", "historical assertion")
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{m}})
	validTo := base.Add(time.Hour)
	closedAt := base.Add(4 * time.Hour)
	write(t, s, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: m.ID, ValidTo: &validTo, ClosedAt: &closedAt}}})
	ms, err := s.Get(ctx, "t", []string{m.ID})
	if err != nil || len(ms) != 1 || ms[0].ClosedAt == nil {
		t.Fatalf("missing system closure: %+v %v", ms, err)
	}
	if !ms[0].ClosedAt.Equal(closedAt) {
		t.Fatalf("provided closure clock ignored: %v", ms[0].ClosedAt)
	}
	for _, tc := range []struct {
		known time.Time
		want  int
	}{{base.Add(2 * time.Hour), 1}, {ms[0].ClosedAt.Add(time.Second), 0}} {
		got, err := s.Find(ctx, store.Query{Tenant: "t", ValidAt: base.Add(3 * time.Hour), KnownAt: tc.known})
		if err != nil || len(got) != tc.want {
			t.Fatalf("known=%v got=%+v err=%v", tc.known, got, err)
		}
		if admitted := (&store.Query{ValidAt: base.Add(3 * time.Hour), KnownAt: tc.known}).Admits(&ms[0], time.Now()); admitted != (tc.want == 1) {
			t.Fatalf("Go temporal predicate disagrees: %v", admitted)
		}
	}
}

func testReceiptPrivacy(t *testing.T, s store.Store) {
	ops, ok := s.(store.Operations)
	if !ok {
		t.Skip("no operation receipts")
	}
	m := mem("t", "erase this secret")
	result, _ := json.Marshal(m)
	b := &store.Batch{Tenant: "t", OperationKey: "private", OperationHash: "h", Result: result, Memories: []memory.Memory{m}}
	write(t, s, b)
	// A failed deletion must not redact a committed result.
	_, err := s.Write(ctx, &store.Batch{Tenant: "t", Deletes: []string{m.ID}, Cursors: []store.Cursor{{Run: "missing", From: 1}}})
	if !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("failed deletion: %v", err)
	}
	if r, err := ops.Operation(ctx, "t", "private"); err != nil || len(r.Result) == 0 {
		t.Fatalf("premature receipt redaction: %+v %v", r, err)
	}
	write(t, s, &store.Batch{Tenant: "other", Deletes: []string{m.ID}})
	if r, err := ops.Operation(ctx, "t", "private"); err != nil || len(r.Result) == 0 {
		t.Fatalf("cross-tenant receipt redaction: %+v %v", r, err)
	}
	write(t, s, &store.Batch{Tenant: "t", Deletes: []string{m.ID}})
	if r, err := ops.Operation(ctx, "t", "private"); err != nil || len(r.Result) != 0 || r.OperationHash != "h" {
		t.Fatalf("missing tombstone: %+v %v", r, err)
	}
	r := write(t, s, b)
	if !r.Replayed || len(r.Result) != 0 {
		t.Fatalf("privacy replay: %+v", r)
	}
	if got, _ := s.Get(ctx, "t", []string{m.ID}); len(got) != 0 {
		t.Fatal("replay recreated erased memory")
	}
}

func testRelationCloses(t *testing.T, s store.Store) {
	rs, ok := s.(relations.Store)
	if !ok {
		t.Skip("no relations")
	}
	a, b := mem("t", "a"), mem("t", "b")
	link := relations.Link{ID: "stable", Source: a.ID, Target: b.ID, Relation: "supports", Origin: "validated", At: base, ValidFrom: base}
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{a, b}, Relations: []relations.Link{link}})
	extra := mem("t", "must rollback")
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Memories: []memory.Memory{extra}, RelationCloses: []relations.Close{{ID: "missing"}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("unknown link close: %v", err)
	}
	if got, _ := s.Get(ctx, "t", []string{extra.ID}); len(got) != 0 {
		t.Fatal("partial relation-close transaction")
	}
	at := base.Add(time.Hour)
	write(t, s, &store.Batch{Tenant: "t", RelationCloses: []relations.Close{{ID: link.ID, At: at}}})
	got, err := rs.RelationshipSets(ctx, "t", []string{a.ID})
	if err != nil || len(got) != 1 || got[0].InvalidatedAt == nil || !got[0].InvalidatedAt.Equal(at) || got[0].Origin != "validated" {
		t.Fatalf("explicit history closure: %+v %v", got, err)
	}
	write(t, s, &store.Batch{Tenant: "t", Relations: []relations.Link{link}})
	got, err = rs.RelationshipSets(ctx, "t", []string{a.ID})
	if err != nil || len(got) != 1 || got[0].InvalidatedAt == nil {
		t.Fatalf("stable append reopened history: %+v %v", got, err)
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", RelationCloses: []relations.Close{{ID: link.ID, At: at}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("closing closed link: %v", err)
	}
}

func testBoundedRelationHistory(t *testing.T, s store.Store) {
	rs, ok := s.(relations.Store)
	if !ok {
		t.Skip("no relations")
	}
	a, b := mem("t", "a"), mem("t", "b")
	links := make([]relations.Link, relations.MaxReadLinks+1)
	for i := range links {
		links[i] = relations.Link{ID: fmt.Sprintf("link-%05d", i), Source: a.ID, Target: b.ID, Relation: "related_to", At: base.Add(time.Duration(i) * time.Second)}
	}
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{a, b}, Relations: links})
	got, err := rs.RelationshipSets(ctx, "t", []string{a.ID, b.ID})
	if err != nil || len(got) != relations.MaxReadLinks {
		t.Fatalf("bounded history count=%d err=%v", len(got), err)
	}
	if got[0].ID != links[len(links)-1].ID || got[len(got)-1].ID != links[1].ID {
		t.Fatalf("history not most-recent first: first=%s last=%s", got[0].ID, got[len(got)-1].ID)
	}
}

func testDependencyClosure(t *testing.T, s store.Store) {
	rs, ok := s.(relations.Store)
	if !ok {
		t.Skip("no relations")
	}
	jobs := func(tenant string) map[string]map[string]any {
		t.Helper()
		js, err := s.Jobs(ctx, store.JobQuery{Tenant: tenant, Kind: "knowledge.reevaluate", Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, j := range js {
			var p map[string]any
			if err := json.Unmarshal(j.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if _, dup := out[p["id"].(string)]; dup {
				t.Fatalf("duplicate job for %v", p["id"])
			}
			var key []string
			if err := json.Unmarshal([]byte(j.Key), &key); err != nil || len(key) != 2 || key[0] != p["id"] || key[1] != p["source"] {
				t.Fatalf("unstable key %q for %s", j.Key, j.Payload)
			}
			out[p["id"].(string)] = p
		}
		return out
	}
	atom, scene, profile, other := mem("t", "atom"), mem("t", "scene"), mem("t", "profile"), mem("t", "other")
	link := func(id string, src, dst memory.Memory, rel string) relations.Link {
		return relations.Link{ID: id, Source: src.ID, Target: dst.ID, Relation: rel, At: base, ValidFrom: base}
	}
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{atom, scene, profile, other}, Relations: []relations.Link{
		link("scene-atom", scene, atom, "derived_from"),
		link("profile-scene", profile, scene, "derived_from"),
		link("atom-profile", atom, profile, "depends_on"), // cycle
		link("other-scene", other, scene, "related_to"),
	}})
	at := base.Add(time.Hour)
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: atom.ID, ValidTo: &at}}, Cursors: []store.Cursor{{Run: "missing", From: 1}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("rollback: %v", err)
	}
	if got := jobs("t"); len(got) != 0 {
		t.Fatalf("failed close enqueued: %v", got)
	}
	write(t, s, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: atom.ID, ValidTo: &at}}})
	got := jobs("t")
	if len(got) != 2 || got[scene.ID]["source"] != atom.ID || got[profile.ID]["source"] != atom.ID ||
		len(got[scene.ID]) != 2 || len(got[profile.ID]) != 2 {
		t.Fatalf("transitive jobs: %v", got)
	}
	links, err := rs.RelationshipSets(ctx, "t", []string{atom.ID, scene.ID, profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		touches := l.Source == atom.ID || l.Target == atom.ID
		if touches != (l.InvalidatedAt != nil) {
			t.Fatalf("link %s retired=%v", l.ID, l.InvalidatedAt != nil)
		}
	}

	// Depth bound: a chain longer than the budget is notified to depth 4 and
	// every job says the notification set is incomplete.
	chain := make([]memory.Memory, store.MaxDependencyDepth+3)
	var chainLinks []relations.Link
	for i := range chain {
		chain[i] = mem("deep", fmt.Sprintf("c%d", i))
		if i > 0 {
			chainLinks = append(chainLinks, link(fmt.Sprintf("c%d", i), chain[i], chain[i-1], "derived_from"))
		}
	}
	write(t, s, &store.Batch{Tenant: "deep", Memories: chain, Relations: chainLinks})
	write(t, s, &store.Batch{Tenant: "deep", Closes: []store.Close{{ID: chain[0].ID, ValidTo: &at}}})
	got = jobs("deep")
	if len(got) != store.MaxDependencyDepth {
		t.Fatalf("depth bound: %d jobs", len(got))
	}
	for i := 1; i <= store.MaxDependencyDepth; i++ {
		if p := got[chain[i].ID]; p == nil || p["truncated"] != true {
			t.Fatalf("depth %d job: %v", i, p)
		}
	}

	// Global job budget across the batch.
	root := mem("wide", "root")
	wide := []memory.Memory{root}
	var wideLinks []relations.Link
	for i := 0; i <= store.MaxDependencyJobs; i++ {
		m := mem("wide", fmt.Sprintf("w%03d", i))
		wide = append(wide, m)
		wideLinks = append(wideLinks, link(m.ID, m, root, "depends_on"))
	}
	write(t, s, &store.Batch{Tenant: "wide", Memories: wide, Relations: wideLinks})
	write(t, s, &store.Batch{Tenant: "wide", Closes: []store.Close{{ID: root.ID, ValidTo: &at}}})
	got = jobs("wide")
	if len(got) != store.MaxDependencyJobs {
		t.Fatalf("job budget: %d jobs", len(got))
	}
	for id, p := range got {
		if p["truncated"] != true {
			t.Fatalf("job %s not marked truncated", id)
		}
	}
}
