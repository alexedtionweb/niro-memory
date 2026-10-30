// Package storetest is the conformance kit for primary-store connectors.
//
// A connector proves it honours the store contract with one test:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) store.Store { return newEmptyStore(t) })
//	}
//
// The kit covers tenant isolation, atomic batches, slot uniqueness, the
// bi-temporal model, filter semantics (compared against memory.Filter.Match),
// episodes, entities, vectors, full-text search, jobs and concurrency.
// Optional capabilities are tested only when the store implements them.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/textutil"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Factory returns a new, empty store. It is called once per subtest.
type Factory func(t *testing.T) store.Store

// Run executes the whole conformance suite.
func Run(t *testing.T, newStore Factory) {
	tests := []struct {
		name string
		fn   func(*testing.T, store.Store)
	}{
		{"TenantIsolation", testIsolation},
		{"AtomicBatch", testAtomic},
		{"Slots", testSlots},
		{"Temporal", testTemporal},
		{"FilterSemantics", testFilters},
		{"OrderAndPaging", testPaging},
		{"Episodes", testEpisodes},
		{"Runs", testRuns},
		{"Entities", testEntities},
		{"DeleteCascade", testDelete},
		{"Touch", testTouch},
		{"Settings", testSettings},
		{"Lexical", testLexical},
		{"Vectors", testVectors},
		{"Jobs", testJobs},
		{"ConcurrentSlotWrites", testConcurrentSlots},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { s.Close() })
			tc.fn(t, s)
		})
	}
}

var ctx = context.Background()

// base is a millisecond-truncated reference time; stores may round to ms.
var base = time.Now().UTC().Truncate(time.Millisecond).Add(-24 * time.Hour)

func mem(tenant, text string, mut ...func(*memory.Memory)) memory.Memory {
	m := memory.Memory{
		ID: memory.NewID(), Scope: memory.Scope{Tenant: tenant, User: "alice", Agent: "a1"},
		Kind: memory.Semantic, Layer: memory.Atom, Text: text, Hash: memory.Hash(text),
		Confidence: 0.8, Importance: 0.5, Visibility: memory.Team,
		ValidFrom: base, RecordedAt: base, Version: 1,
		Source: memory.Source{Kind: "explicit", Trust: memory.TrustHigh},
	}
	for _, f := range mut {
		f(&m)
	}
	return m
}

func write(t *testing.T, s store.Store, b *store.Batch) *store.Receipt {
	t.Helper()
	r, err := s.Write(ctx, b)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	return r
}

func ids(ms []memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tp(t time.Time) *time.Time { return &t }

func testIsolation(t *testing.T, s store.Store) {
	a := mem("ta", "Alice likes green tea", func(m *memory.Memory) {
		m.Entities = []memory.EntityRef{{Type: "drink", Name: "Green Tea"}}
		m.Slot = "user.drink"
	})
	write(t, s, &store.Batch{Tenant: "ta", Memories: []memory.Memory{a},
		Events:   []memory.Event{{MemoryID: a.ID, Action: memory.ActionAdd, New: a.Text, At: base}},
		Episodes: []memory.Episode{{Scope: memory.Scope{Run: "r1"}, ClientMsgID: "c1", Role: "user", OccurredAt: base, Parts: []memory.Part{{Kind: memory.PartText, Text: "green tea please"}}}},
		Settings: []byte(`{"x":1}`)})
	// The same slot and client id in another tenant must not conflict.
	b := mem("tb", "Bob likes coffee", func(m *memory.Memory) { m.Slot = "user.drink" })
	write(t, s, &store.Batch{Tenant: "tb", Memories: []memory.Memory{b},
		Episodes: []memory.Episode{{Scope: memory.Scope{Run: "r1"}, ClientMsgID: "c1", Role: "user", OccurredAt: base, Parts: []memory.Part{{Kind: memory.PartText, Text: "coffee"}}}}})

	if got, _ := s.Get(ctx, "tb", []string{a.ID}); len(got) != 0 {
		t.Fatal("Get leaked across tenants")
	}
	if got, _ := s.Find(ctx, store.Query{Tenant: "tb"}); !eq(ids(got), []string{b.ID}) {
		t.Fatalf("Find leaked across tenants: %v", ids(got))
	}
	if h, _ := s.History(ctx, "tb", a.ID); len(h) != 0 {
		t.Fatal("History leaked across tenants")
	}
	eps, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "tb", Run: "r1"})
	if len(eps) != 1 || eps[0].Text() != "coffee" || eps[0].Seq != 1 {
		t.Fatalf("Episodes leaked or wrong seq: %+v", eps)
	}
	if st, _ := s.Settings(ctx, "tb"); len(st) != 0 {
		t.Fatal("Settings leaked across tenants")
	}
	if lx, ok := s.(store.Lexical); ok {
		if hits, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "tb"}, Terms: []string{"tea"}}); len(hits) != 0 {
			t.Fatal("SearchMemories leaked across tenants")
		}
		if hits, _ := lx.SearchEpisodes(ctx, store.EpisodeTextQuery{EpisodeQuery: store.EpisodeQuery{Tenant: "tb"}, Terms: []string{"tea"}}); len(hits) != 0 {
			t.Fatal("SearchEpisodes leaked across tenants")
		}
	}
	if en, ok := s.(store.Entities); ok {
		if es, _ := en.FindEntities(ctx, "tb", []string{"green tea"}); len(es) != 0 {
			t.Fatal("FindEntities leaked across tenants")
		}
	}
	if _, err := s.Run(ctx, "tc", "r1"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Run in unknown tenant: %v", err)
	}
}

func testAtomic(t *testing.T, s store.Store) {
	m := mem("t", "first")
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{m}})
	bad := &store.Batch{Tenant: "t",
		Memories: []memory.Memory{mem("t", "should not exist")},
		Episodes: []memory.Episode{{Scope: memory.Scope{Run: "r"}, Role: "user", OccurredAt: base, Parts: []memory.Part{{Kind: memory.PartText, Text: "x"}}}},
		Jobs:     []store.Job{{Kind: "k", Key: "x"}},
		Closes:   []store.Close{{ID: "does-not-exist", ValidTo: tp(base)}},
	}
	if _, err := s.Write(ctx, bad); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	all, _ := s.Find(ctx, store.Query{Tenant: "t", AllVersions: true})
	if !eq(ids(all), []string{m.ID}) {
		t.Fatalf("failed batch was partially applied: %v", ids(all))
	}
	if eps, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t"}); len(eps) != 0 {
		t.Fatal("failed batch inserted episodes")
	}
	if js, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t"}); len(js) != 0 {
		t.Fatal("failed batch enqueued jobs")
	}
	if _, err := s.Write(ctx, &store.Batch{}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("missing tenant: want ErrInvalid, got %v", err)
	}
}

func testSlots(t *testing.T, s store.Store) {
	slot := func(m *memory.Memory) { m.Slot = "user.city" }
	paris := mem("t", "Alice lives in Paris", slot)
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{paris}})
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Memories: []memory.Memory{mem("t", "Alice lives in Rome", slot)}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("second open slot value: want ErrConflict, got %v", err)
	}
	// Other user or agent: separate slot.
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{mem("t", "Bob lives in Rome", slot, func(m *memory.Memory) { m.User = "bob" })}})
	// Supersede in one batch: close old + insert new.
	moved := base.Add(time.Hour)
	lisbon := mem("t", "Alice lives in Lisbon", slot, func(m *memory.Memory) { m.ValidFrom, m.Supersedes, m.Version = moved, paris.ID, 2 })
	write(t, s, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: paris.ID, ValidTo: &moved}}, Memories: []memory.Memory{lisbon}})
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Closes: []store.Close{{ID: paris.ID, InvalidatedAt: &moved}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("closing a closed memory: want ErrConflict, got %v", err)
	}
	got, _ := s.Get(ctx, "t", []string{paris.ID, lisbon.ID})
	for _, m := range got {
		if m.ID == paris.ID && (m.ValidTo == nil || !m.ValidTo.Equal(moved)) {
			t.Fatalf("paris not closed: %+v", m.ValidTo)
		}
		if m.ID == lisbon.ID && m.Supersedes != paris.ID {
			t.Fatal("supersedes not stored")
		}
	}
	// Closed (non-open) memories never conflict.
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{mem("t", "Alice lived in Oslo", slot, func(m *memory.Memory) { m.ValidTo = tp(base.Add(time.Minute)) })}})
}

func testTemporal(t *testing.T, s store.Store) {
	t1, t2, t3 := base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour)
	old := mem("t", "v1", func(m *memory.Memory) { m.ValidTo = tp(t2) })
	cur := mem("t", "v2", func(m *memory.Memory) { m.ValidFrom = t2 })
	wrong := mem("t", "retracted", func(m *memory.Memory) { m.InvalidatedAt = tp(t3) })
	expired := mem("t", "expired", func(m *memory.Memory) { m.ExpiresAt = tp(base.Add(time.Minute)) })
	future := mem("t", "future", func(m *memory.Memory) { m.ValidFrom = time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond) })
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{old, cur, wrong, expired, future}})
	cases := []struct {
		name string
		q    store.Query
		want []string
	}{
		{"now", store.Query{}, []string{cur.ID}},
		{"valid at t1", store.Query{ValidAt: t1}, []string{old.ID}},
		{"valid at t1 known at t1", store.Query{ValidAt: t1, KnownAt: t1}, []string{old.ID, wrong.ID}},
		{"show expired", store.Query{ShowExpired: true}, []string{cur.ID, expired.ID}},
		{"all versions", store.Query{AllVersions: true}, []string{old.ID, cur.ID, wrong.ID, future.ID}},
		{"all versions with expired", store.Query{AllVersions: true, ShowExpired: true}, []string{old.ID, cur.ID, wrong.ID, expired.ID, future.ID}},
	}
	for _, c := range cases {
		c.q.Tenant = "t"
		got, err := s.Find(ctx, c.q)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if err != nil || !eq(ids(got), want) {
			t.Errorf("%s: got %v want %v (%v)", c.name, ids(got), want, err)
		}
	}
	// The shared Go definition must agree with the store.
	all, _ := s.Find(ctx, store.Query{Tenant: "t", AllVersions: true, ShowExpired: true})
	q := store.Query{ValidAt: t1, KnownAt: t1}
	var viaGo []memory.Memory
	for i := range all {
		if q.Admits(&all[i], time.Now()) {
			viaGo = append(viaGo, all[i])
		}
	}
	if !eq(ids(viaGo), []string{old.ID, wrong.ID}) {
		t.Errorf("store.Query.Admits disagrees: %v", ids(viaGo))
	}
}

func testFilters(t *testing.T, s store.Store) {
	ms := []memory.Memory{
		mem("t", "Alice lives in Lisbon", func(m *memory.Memory) {
			m.Tags, m.Metadata = []string{"home", "travel"}, map[string]any{"city": "Lisbon", "n": 3.0, "ok": true}
			m.Entities = []memory.EntityRef{{Type: "place", Name: "Lisbon"}}
			m.Slot, m.Subject, m.Layer = "user.city", "alice", memory.Atom
		}),
		mem("t", "Alice prefers tea", func(m *memory.Memory) {
			m.Tags, m.Metadata, m.Confidence = []string{"food"}, map[string]any{"n": 10.0}, 0.4
			m.Kind, m.Visibility, m.Agent = memory.Episodic, memory.Private, ""
		}),
		mem("t", "Bob owns a dog named REX", func(m *memory.Memory) {
			m.User, m.Visibility, m.Grants = "bob", memory.Restricted, []string{"role:vet", "user:carol"}
			m.Layer, m.Run, m.Heat = memory.Profile, "run-9", 4
			m.ExpiresAt = tp(time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond))
		}),
		mem("t", "Team standup is at 9", func(m *memory.Memory) { m.User, m.Agent, m.Kind = "", "", memory.Procedural }),
	}
	write(t, s, &store.Batch{Tenant: "t", Memories: ms})
	filters := []string{
		`{}`,
		`{"user_id":"alice"}`, `{"user_id":""}`, `{"agent_id":{"ne":"a1"}}`,
		`{"kind":{"in":["episodic","procedural"]}}`, `{"kind":{"nin":["semantic"]}}`,
		`{"text":{"contains":"tea"}}`, `{"text":{"icontains":"rex"}}`, `{"text":{"contains":"REX"}}`,
		`{"tags":"travel"}`, `{"tags":{"contains":"food"}}`, `{"tags":{"in":["food","home"]}}`, `{"tags":{"nin":["home"]}}`, `{"tags":{"ne":"home"}}`,
		`{"grants":{"in":["role:vet"]}}`, `{"entities":"lisbon"}`,
		`{"city":"Lisbon"}`, `{"metadata.n":{"gt":3}}`, `{"metadata.n":{"gte":3}}`, `{"metadata.n":{"lt":5}}`, `{"metadata.ok":true}`,
		`{"metadata.city":{"exists":true}}`, `{"metadata.city":{"exists":false}}`, `{"metadata.city":{"ne":"Lisbon"}}`,
		`{"confidence":{"lt":0.5}}`, `{"layer":3}`, `{"heat":{"gte":1}}`, `{"version":1}`,
		`{"expires_at":{"exists":true}}`, `{"valid_to":{"exists":false}}`, `{"created_at":{"gte":"2000-01-01"}}`,
		`{"recorded_at":{"lt":"2000-01-01"}}`, `{"run_id":"run-9"}`, `{"slot":"user.city"}`, `{"subject":{"exists":true}}`,
		`{"OR":[{"user_id":"bob"},{"kind":"procedural"}]}`,
		`{"NOT":{"user_id":"alice"}}`,
		`{"AND":[{"user_id":"alice"},{"OR":[{"tags":"food"},{"metadata.n":{"lte":3}}]}]}`,
	}
	acl := []memory.Principal{{User: "alice", Agent: "a1"}, {User: "bob", Roles: []string{"vet"}}, {User: "carol"}, {User: "dave"}}
	all, _ := s.Find(ctx, store.Query{Tenant: "t", AllVersions: true, ShowExpired: true})
	check := func(name string, f memory.Filter) {
		t.Helper()
		got, err := s.Find(ctx, store.Query{Tenant: "t", Filter: f})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		var want []memory.Memory
		for i := range all {
			if f.Match(&all[i]) {
				want = append(want, all[i])
			}
		}
		if !eq(ids(got), ids(want)) {
			t.Errorf("%s: store returned %d, Filter.Match %d", name, len(got), len(want))
		}
	}
	for _, js := range filters {
		f, err := memory.ParseFilter([]byte(js))
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		check(js, f)
	}
	for _, p := range acl {
		check(fmt.Sprintf("readable by %+v", p), memory.Readable(p))
	}
}

func testPaging(t *testing.T, s store.Store) {
	var ms []memory.Memory
	for i := 0; i < 7; i++ {
		ms = append(ms, mem("t", fmt.Sprintf("memory %d", i), func(m *memory.Memory) { m.Heat = int64(i % 3) }))
	}
	write(t, s, &store.Batch{Tenant: "t", Memories: ms})
	newest, _ := s.Find(ctx, store.Query{Tenant: "t", Limit: 3})
	if len(newest) != 3 || newest[0].ID != ms[6].ID || newest[2].ID != ms[4].ID {
		t.Fatalf("newest first: %v", ids(newest))
	}
	next, _ := s.Find(ctx, store.Query{Tenant: "t", Limit: 3, Offset: 3})
	if len(next) != 3 || next[0].ID != ms[3].ID {
		t.Fatalf("offset paging: %v", ids(next))
	}
	oldest, _ := s.Find(ctx, store.Query{Tenant: "t", Order: store.OldestFirst, Limit: 1})
	if len(oldest) != 1 || oldest[0].ID != ms[0].ID {
		t.Fatal("oldest first")
	}
	hot, _ := s.Find(ctx, store.Query{Tenant: "t", Order: store.HottestFirst, Limit: 2})
	if len(hot) != 2 || hot[0].Heat != 2 || hot[1].Heat != 2 {
		t.Fatalf("hottest first: %+v", hot)
	}
	full, _ := s.Get(ctx, "t", []string{ms[0].ID})
	if len(full) != 1 || full[0].Text != "memory 0" || full[0].Confidence != 0.8 || full[0].Source.Trust != memory.TrustHigh || !full[0].ValidFrom.Equal(base) {
		t.Fatalf("round trip lost data: %+v", full)
	}
}

func ep(run, cid, role, text string, at time.Time) memory.Episode {
	return memory.Episode{Scope: memory.Scope{User: "alice", Agent: "a1", Run: run}, ClientMsgID: cid, Role: role, OccurredAt: at,
		Parts: []memory.Part{{Kind: memory.PartText, Text: text}}, Metadata: map[string]any{"k": "v"}}
}

func testEpisodes(t *testing.T, s store.Store) {
	blob := &memory.BlobRef{SHA256: "ab" + fmt.Sprintf("%062d", 1), Mime: "image/png", Size: 10}
	img := ep("r1", "m3", "user", "", base.Add(2*time.Second))
	img.Parts = []memory.Part{{Kind: memory.PartImage, Blob: blob, Caption: "a red bicycle"}, {Kind: memory.PartToolCall, Name: "lookup", Data: json.RawMessage(`{"q":1}`)}}
	r := write(t, s, &store.Batch{Tenant: "t", Episodes: []memory.Episode{
		ep("r1", "m1", "user", "hello there", base), ep("r1", "m2", "assistant", "hi alice", base.Add(time.Second)), img,
		ep("r2", "m1", "user", "other run", base),
	}})
	if len(r.Episodes) != 4 || r.Episodes[2].Episode.Seq != 3 || r.Episodes[3].Episode.Seq != 1 || r.Episodes[0].Duplicate {
		t.Fatalf("seq assignment: %+v", r.Episodes)
	}
	again := write(t, s, &store.Batch{Tenant: "t", Episodes: []memory.Episode{ep("r1", "m2", "assistant", "changed", base), ep("r1", "m4", "user", "new", base.Add(3*time.Second))}})
	if !again.Episodes[0].Duplicate || again.Episodes[0].Episode.Text() != "hi alice" || again.Episodes[1].Episode.Seq != 4 {
		t.Fatalf("idempotent append: %+v", again.Episodes)
	}
	all, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Run: "r1"})
	if len(all) != 4 || all[0].Seq != 1 || all[3].Seq != 4 {
		t.Fatalf("run order: %d", len(all))
	}
	if all[2].Parts[0].Blob == nil || all[2].Parts[0].Blob.SHA256 != blob.SHA256 || all[2].Parts[1].Name != "lookup" || string(all[2].Parts[1].Data) != `{"q":1}` || all[0].Metadata["k"] != "v" {
		t.Fatalf("parts round trip: %+v", all[2].Parts)
	}
	last2, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Run: "r1", Desc: true, Limit: 2})
	if len(last2) != 2 || last2[0].Seq != 4 || last2[1].Seq != 3 {
		t.Fatal("desc + limit")
	}
	window, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Run: "r1", AfterSeq: 1, BeforeSeq: 4})
	if len(window) != 2 || window[0].Seq != 2 {
		t.Fatal("seq window")
	}
	users, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Roles: []string{"assistant"}})
	if len(users) != 1 {
		t.Fatal("roles filter")
	}
	since, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Run: "r1", Since: base.Add(time.Second), Until: base.Add(3 * time.Second)})
	if len(since) != 2 {
		t.Fatalf("time window: %d", len(since))
	}
	byBlob, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", BlobSHA: blob.SHA256})
	if len(byBlob) != 1 || byBlob[0].ClientMsgID != "m3" {
		t.Fatal("blob lookup")
	}
	byID, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", IDs: []string{all[0].ID}})
	if len(byID) != 1 {
		t.Fatal("ids filter")
	}
	write(t, s, &store.Batch{Tenant: "t", EpisodeEdits: []store.EpisodeEdit{
		{ID: all[0].ID, Status: memory.Edited, Parts: []memory.Part{{Kind: memory.PartText, Text: "hello world"}}},
		{ID: all[1].ID, Status: memory.Deleted},
	}})
	vis, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Run: "r1"})
	if len(vis) != 3 || vis[0].Text() != "hello world" || vis[0].Status != memory.Edited {
		t.Fatalf("edits: %+v", vis)
	}
	if inc, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", Run: "r1", IncludeDeleted: true}); len(inc) != 4 {
		t.Fatal("include deleted")
	}
	write(t, s, &store.Batch{Tenant: "t", DeleteRuns: []string{"r1"}})
	if left, _ := s.Episodes(ctx, store.EpisodeQuery{Tenant: "t", IncludeDeleted: true}); len(left) != 1 || left[0].Run != "r2" {
		t.Fatal("delete run")
	}
	if _, err := s.Run(ctx, "t", "r1"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal("deleted run still exists")
	}
	// A deleted run starts over and client ids are free again.
	re := write(t, s, &store.Batch{Tenant: "t", Episodes: []memory.Episode{ep("r1", "m1", "user", "again", base)}})
	if re.Episodes[0].Duplicate || re.Episodes[0].Episode.Seq != 1 {
		t.Fatal("run restart")
	}
}

func testRuns(t *testing.T, s store.Store) {
	write(t, s, &store.Batch{Tenant: "t", Episodes: []memory.Episode{ep("r", "1", "user", "a", base), ep("r", "2", "user", "b", base)}})
	run, err := s.Run(ctx, "t", "r")
	if err != nil || run.LastSeq != 2 || run.ProcessedSeq != 0 || run.User != "alice" {
		t.Fatalf("run state: %+v %v", run, err)
	}
	write(t, s, &store.Batch{Tenant: "t", Cursors: []store.Cursor{{Run: "r", From: 0, To: 2}}, Summaries: []store.Summary{{Run: "r", Text: "greetings", Upto: 2}}})
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Cursors: []store.Cursor{{Run: "r", From: 0, To: 1}}}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("stale cursor: want ErrConflict, got %v", err)
	}
	run, _ = s.Run(ctx, "t", "r")
	if run.ProcessedSeq != 2 || run.Summary != "greetings" || run.SummaryUpto != 2 {
		t.Fatalf("cursor/summary: %+v", run)
	}
}

func testEntities(t *testing.T, s store.Store) {
	en, ok := s.(store.Entities)
	if !ok {
		t.Skip("store does not implement store.Entities")
	}
	a := mem("t", "Alice met Bob in Lisbon", func(m *memory.Memory) {
		m.Entities = []memory.EntityRef{{Type: "person", Name: "Bob"}, {Type: "place", Name: "Lisbon"}}
	})
	b := mem("t", "Bob works at ACME Corp.", func(m *memory.Memory) {
		m.Entities = []memory.EntityRef{{Type: "person", Name: "bob"}, {Type: "org", Name: "Acme-Corp"}}
	})
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{a, b}})
	write(t, s, &store.Batch{Tenant: "t", Aliases: []store.Alias{{EntityKey: "acme corp", Name: "Acme Corporation"}}})
	es, err := en.FindEntities(ctx, "t", []string{"bob", "acme corporation", "nobody"})
	if err != nil || len(es) != 2 {
		t.Fatalf("FindEntities: %+v %v", es, err)
	}
	var bob memory.Entity
	for _, e := range es {
		if e.Key == "bob" {
			bob = e
		}
		if e.Key == "acme corp" && (len(e.Aliases) != 1 || e.Aliases[0] != "Acme Corporation") {
			t.Fatalf("alias not stored: %+v", e)
		}
	}
	if bob.Mentions != 2 || bob.Name != "Bob" {
		t.Fatalf("entity merge by key: %+v", bob)
	}
	links, _ := en.EntityMemories(ctx, "t", []string{bob.ID}, 10)
	if len(links) != 2 || links[0].MemoryID != b.ID {
		t.Fatalf("EntityMemories newest first: %+v", links)
	}
	if one, _ := en.EntityMemories(ctx, "t", []string{bob.ID}, 1); len(one) != 1 {
		t.Fatal("EntityMemories limit")
	}
	got, _ := s.Get(ctx, "t", []string{a.ID})
	if len(got[0].Entities) != 2 {
		t.Fatalf("Get must populate entities: %+v", got[0].Entities)
	}
	list, _ := en.ListEntities(ctx, "t", "", 10)
	if len(list) != 3 || list[0].Key != "bob" {
		t.Fatalf("ListEntities most mentioned first: %+v", list)
	}
	if pre, _ := en.ListEntities(ctx, "t", "li", 10); len(pre) != 1 {
		t.Fatal("ListEntities prefix")
	}
}

func testDelete(t *testing.T, s store.Store) {
	m := mem("t", "secret thing about Zed", func(m *memory.Memory) {
		m.Entities = []memory.EntityRef{{Name: "Zed"}}
		m.Slot = "user.secret"
	})
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{m},
		Events:  []memory.Event{{MemoryID: m.ID, Action: memory.ActionAdd, New: m.Text, At: base}},
		Vectors: []vector.Point{{Model: "m1", Target: vector.Target{Kind: vector.TargetMemory, ID: m.ID}, Values: []float32{1, 0}}}})
	write(t, s, &store.Batch{Tenant: "t", Deletes: []string{m.ID}, Events: []memory.Event{{MemoryID: m.ID, Action: memory.ActionDelete, At: base}}})
	if got, _ := s.Get(ctx, "t", []string{m.ID}); len(got) != 0 {
		t.Fatal("memory not deleted")
	}
	h, _ := s.History(ctx, "t", m.ID)
	if len(h) != 2 || h[0].New != "" || h[1].Action != memory.ActionDelete {
		t.Fatalf("history must keep the audit trail without content: %+v", h)
	}
	if en, ok := s.(store.Entities); ok {
		es, _ := en.FindEntities(ctx, "t", []string{"zed"})
		if len(es) == 1 {
			if links, _ := en.EntityMemories(ctx, "t", []string{es[0].ID}, 10); len(links) != 0 {
				t.Fatal("entity links not deleted")
			}
		}
	}
	if vc, ok := s.(store.VectorCatalog); ok {
		if pts, _ := vc.Vectors(ctx, "t", "m1", "", 10); len(pts) != 0 {
			t.Fatal("vectors not deleted")
		}
	}
	// The slot is free again.
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{mem("t", "new secret", func(m *memory.Memory) { m.Slot = "user.secret" })}})
}

func testTouch(t *testing.T, s store.Store) {
	m := mem("t", "seen often")
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{m}})
	write(t, s, &store.Batch{Tenant: "t", Touches: []store.Touch{{ID: m.ID, Heat: 2, Evidence: []memory.Evidence{{Episode: "e1", Start: 1, End: 4}}}}})
	write(t, s, &store.Batch{Tenant: "t", Touches: []store.Touch{{ID: m.ID, Heat: 1}, {ID: "missing", Heat: 1}}})
	got, _ := s.Get(ctx, "t", []string{m.ID})
	if got[0].Heat != 3 || len(got[0].Source.Evidence) != 1 || got[0].Source.Evidence[0].End != 4 {
		t.Fatalf("touch: %+v", got[0])
	}
}

func testSettings(t *testing.T, s store.Store) {
	if st, err := s.Settings(ctx, "t"); err != nil || len(st) != 0 {
		t.Fatalf("empty settings: %s %v", st, err)
	}
	write(t, s, &store.Batch{Tenant: "t", Settings: []byte(`{"a":1}`)})
	write(t, s, &store.Batch{Tenant: "t", Settings: []byte(`{"a":2}`)})
	if st, _ := s.Settings(ctx, "t"); string(st) != `{"a":2}` {
		t.Fatalf("settings: %s", st)
	}
}

func testLexical(t *testing.T, s store.Store) {
	lx, ok := s.(store.Lexical)
	if !ok {
		t.Skip("store does not implement store.Lexical")
	}
	a := mem("t", "The quick brown fox jumps over the lazy dog")
	b := mem("t", "Fox fox fox: a report about foxes and the fox den")
	c := mem("t", "Unrelated text about databases", func(m *memory.Memory) { m.User = "bob" })
	d := mem("t", "old fox fact", func(m *memory.Memory) { m.ValidTo = tp(base.Add(time.Minute)) })
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{a, b, c, d}})
	hits, err := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t", Limit: 10}, Terms: textutil.Terms("fox", 0)})
	if err != nil || len(hits) != 2 || hits[0].ID != b.ID || hits[0].Score <= hits[1].Score || hits[1].Score <= 0 {
		t.Fatalf("ranking: %+v %v", hits, err)
	}
	f, _ := memory.ParseFilter([]byte(`{"user_id":"bob"}`))
	if h, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t", Filter: f}, Terms: []string{"fox", "databases"}}); len(h) != 1 || h[0].ID != c.ID {
		t.Fatalf("filtered search: %+v", h)
	}
	if h, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t", AllVersions: true}, Terms: []string{"fox"}}); len(h) != 3 {
		t.Fatalf("all versions search: %+v", h)
	}
	if h, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t", Limit: 1}, Terms: []string{"fox"}}); len(h) != 1 {
		t.Fatal("limit")
	}
	if h, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t"}, Terms: []string{"repor", "jump"}}); len(h) != 2 {
		t.Fatalf("terms of %d+ characters must prefix-match (repor→report, jump→jumps): %+v", store.PrefixMin, h)
	}
	if h, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t"}, Terms: []string{"fo"}}); len(h) != 0 {
		t.Fatalf("short terms must match whole words only: %+v", h)
	}
	if h, _ := lx.SearchMemories(ctx, store.TextQuery{Query: store.Query{Tenant: "t"}, Terms: []string{"\"or", "near(", "*"}}); len(h) != 0 {
		t.Fatal("hostile terms must not match or error")
	}
	img := ep("r", "1", "user", "", base)
	img.Parts = []memory.Part{{Kind: memory.PartImage, Caption: "a red bicycle in Lisbon"}}
	write(t, s, &store.Batch{Tenant: "t", Episodes: []memory.Episode{ep("r", "2", "assistant", "bicycles are great", base), img}})
	eh, err := lx.SearchEpisodes(ctx, store.EpisodeTextQuery{EpisodeQuery: store.EpisodeQuery{Tenant: "t", Roles: []string{"user"}}, Terms: []string{"bicycle"}})
	if err != nil || len(eh) != 1 || eh[0].Episode.Parts[0].Caption == "" {
		t.Fatalf("episode search with caption: %+v %v", eh, err)
	}
}

func testVectors(t *testing.T, s store.Store) {
	vc, ok := s.(store.VectorCatalog)
	if !ok {
		t.Skip("store does not implement store.VectorCatalog")
	}
	a, b, c := mem("t", "a"), mem("t", "b"), mem("t", "c", func(m *memory.Memory) { m.User = "bob" })
	pt := func(m memory.Memory, v ...float32) vector.Point {
		return vector.Point{Model: "m", Target: vector.Target{Kind: vector.TargetMemory, ID: m.ID}, User: m.User, Values: v}
	}
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{a, b, c}, Vectors: []vector.Point{pt(a, 1, 0, 0), pt(b, 0.9, 0.1, 0), pt(c, 0, 0, 3)}})
	write(t, s, &store.Batch{Tenant: "other", Vectors: []vector.Point{{Model: "m", Target: vector.Target{Kind: vector.TargetMemory, ID: "x"}, Values: []float32{1, 0, 0}}}})
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Vectors: []vector.Point{pt(a, 0, 0, 0)}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("zero vector: want ErrInvalid, got %v", err)
	}
	pts, _ := vc.Vectors(ctx, "t", "m", "", 10)
	if len(pts) != 3 {
		t.Fatalf("Vectors: %d", len(pts))
	}
	if have, total, _ := vc.Coverage(ctx, "t", "m"); have != 3 || total != 3 {
		t.Fatalf("coverage %d/%d", have, total)
	}
	d := mem("t", "d")
	write(t, s, &store.Batch{Tenant: "t", Memories: []memory.Memory{d}})
	if miss, _ := vc.Missing(ctx, "t", "m", 10); len(miss) != 1 || miss[0].ID != d.ID {
		t.Fatalf("Missing: %+v", miss)
	}
	if idx, ok := s.(vector.Index); ok {
		hits, err := idx.Search(ctx, vector.Query{Tenant: "t", Model: "m", Values: []float32{1, 0, 0}, K: 2})
		if err != nil || len(hits) != 2 || hits[0].Target.ID != a.ID || hits[0].Score < 0.999 {
			t.Fatalf("Search: %+v %v", hits, err)
		}
		bob, _ := idx.Search(ctx, vector.Query{Tenant: "t", Model: "m", Values: []float32{1, 0, 0}, K: 5, User: "bob"})
		if len(bob) != 1 || bob[0].Target.ID != c.ID || bob[0].Score > 0.01 {
			t.Fatalf("scoped search: %+v", bob)
		}
		other, _ := idx.Search(ctx, vector.Query{Tenant: "t", Model: "m", Values: []float32{1, 0, 0}, K: 10})
		for _, h := range other {
			if h.Target.ID == "x" {
				t.Fatal("vector search leaked across tenants")
			}
		}
		write(t, s, &store.Batch{Tenant: "t", VectorDeletes: []vector.Target{{Kind: vector.TargetMemory, ID: a.ID}}})
		if h, _ := idx.Search(ctx, vector.Query{Tenant: "t", Model: "m", Values: []float32{1, 0, 0}, K: 1}); len(h) != 1 || h[0].Target.ID != b.ID {
			t.Fatalf("after delete: %+v", h)
		}
	}
}

func testJobs(t *testing.T, s store.Store) {
	now := time.Now()
	write(t, s, &store.Batch{Tenant: "t1", Jobs: []store.Job{{Kind: "process", Key: "r1", Payload: json.RawMessage(`{"run":"r1"}`)}}})
	write(t, s, &store.Batch{Tenant: "t1", Jobs: []store.Job{{Kind: "process", Key: "r1", Payload: json.RawMessage(`{"run":"other"}`)}, {Kind: "process", Key: "r2"}}})
	write(t, s, &store.Batch{Tenant: "t2", Jobs: []store.Job{{Kind: "process", Key: "r1"}, {Kind: "later", Key: "x", NotBefore: now.Add(time.Hour)}}})
	pending, _ := s.Jobs(ctx, store.JobQuery{State: store.JobPending})
	if len(pending) != 4 {
		t.Fatalf("dedup by key: %d pending", len(pending))
	}
	got, err := s.Claim(ctx, "w1", 2, time.Minute)
	if err != nil || len(got) != 2 || got[0].Tenant == got[1].Tenant {
		t.Fatalf("claim must be fair across tenants: %+v %v", got, err)
	}
	for _, j := range got {
		if j.State != store.JobLeased || j.Worker != "w1" || j.Attempts != 1 {
			t.Fatalf("leased job: %+v", j)
		}
		if j.Tenant == "t1" && j.Key == "r1" && string(j.Payload) != `{"run":"r1"}` {
			t.Fatalf("merged job must keep first payload: %s", j.Payload)
		}
	}
	// Re-enqueueing a leased key creates a new pending job, but it is not
	// claimable while the lease is live.
	var leasedT1 store.Job
	for _, j := range got {
		if j.Tenant == "t1" {
			leasedT1 = j
		}
	}
	write(t, s, &store.Batch{Tenant: "t1", Jobs: []store.Job{{Kind: leasedT1.Kind, Key: leasedT1.Key}}})
	more, _ := s.Claim(ctx, "w2", 10, time.Minute)
	for _, j := range more {
		if j.Tenant == "t1" && j.Key == leasedT1.Key {
			t.Fatal("claimed a key that holds a live lease")
		}
		if j.Kind == "later" {
			t.Fatal("claimed a job before NotBefore")
		}
	}
	if err := s.Heartbeat(ctx, leasedT1.ID, "w2", time.Minute, nil); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("heartbeat by wrong worker: %v", err)
	}
	if err := s.Heartbeat(ctx, leasedT1.ID, "w1", time.Minute, json.RawMessage(`{"step":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, leasedT1.ID, "w1", store.Outcome{Error: "boom", RetryAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	retried, _ := s.Jobs(ctx, store.JobQuery{Tenant: "t1", Kind: leasedT1.Kind})
	found := false
	for _, j := range retried {
		if j.ID == leasedT1.ID {
			found = true
			if j.State != store.JobPending || j.LastError != "boom" || string(j.Checkpoint) != `{"step":2}` {
				t.Fatalf("retry state: %+v", j)
			}
		}
	}
	if !found {
		t.Fatal("retried job missing")
	}
	for _, j := range more {
		if err := s.Finish(ctx, j.ID, "w2", store.Outcome{Done: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, more[0].ID, "w2", store.Outcome{Done: true}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("finishing twice: %v", err)
	}
	// Expired leases are reclaimable (crashed worker).
	exp, _ := s.Claim(ctx, "w3", 10, time.Millisecond)
	if len(exp) == 0 {
		t.Fatal("nothing claimable")
	}
	time.Sleep(20 * time.Millisecond)
	re, _ := s.Claim(ctx, "w4", 10, time.Minute)
	reclaimed := false
	for _, j := range re {
		if j.ID == exp[0].ID && j.Attempts == exp[0].Attempts+1 && j.Worker == "w4" {
			reclaimed = true
		}
	}
	if !reclaimed {
		t.Fatalf("expired lease of %s not reclaimed: %+v", exp[0].ID, re)
	}
	if err := s.Finish(ctx, exp[0].ID, "w3", store.Outcome{Done: true}); !errors.Is(err, memory.ErrConflict) {
		t.Fatal("a worker whose lease was taken over must not finish the job")
	}
	if err := s.Finish(ctx, re[0].ID, "w4", store.Outcome{Dead: true, Error: "poison"}); err != nil {
		t.Fatal(err)
	}
	if dead, _ := s.Jobs(ctx, store.JobQuery{State: store.JobDead}); len(dead) != 1 || dead[0].LastError != "poison" {
		t.Fatalf("dead jobs: %+v", dead)
	}
}

func testConcurrentSlots(t *testing.T, s store.Store) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflicts := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := mem("t", fmt.Sprintf("value %d", i), func(m *memory.Memory) { m.Slot = "user.x" })
			_, err := s.Write(ctx, &store.Batch{Tenant: "t", Memories: []memory.Memory{m}})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, memory.ErrConflict):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conflicts != 7 {
		t.Fatalf("exactly one concurrent slot write must win: ok=%d conflicts=%d", ok, conflicts)
	}
}
