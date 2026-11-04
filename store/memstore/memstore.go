// Package memstore is the in-process reference implementation of every
// store interface. It is used by tests, by the conformance kit as a
// baseline, and for ephemeral deployments. It keeps nothing on disk.
package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/textutil"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Store is a thread-safe in-memory store. The zero value is not usable.
type Store struct {
	mu      sync.RWMutex
	now     func() time.Time
	tenants map[string]*tenant
	jobs    map[string]*store.Job
	vec     *vector.Flat
}

type tenant struct {
	mems          map[string]*memory.Memory
	slots         map[string]string // slot key -> open memory id
	history       map[string][]memory.Event
	episodes      map[string]*memory.Episode
	runEps        map[string][]string // run -> episode ids in seq order
	clientIDs     map[string]string   // run\x00client id -> episode id
	runs          map[string]*memory.Run
	entities      map[string]*memory.Entity // by key
	byID          map[string]*memory.Entity
	aliases       map[string]string          // alias key -> entity key
	links         map[string]map[string]bool // entity id -> memory ids
	memEnts       map[string][]string        // memory id -> entity ids
	vectors       map[string]map[string]vector.Point
	settings      []byte
	relationships map[string]relations.Link
	operations    map[string]*store.Receipt
}

// New returns an empty store. now may be nil (time.Now).
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{now: now, tenants: map[string]*tenant{}, jobs: map[string]*store.Job{}, vec: vector.NewFlat()}
}

func (s *Store) t(name string) *tenant {
	t := s.tenants[name]
	if t == nil {
		t = &tenant{
			mems: map[string]*memory.Memory{}, slots: map[string]string{}, history: map[string][]memory.Event{},
			episodes: map[string]*memory.Episode{}, runEps: map[string][]string{}, clientIDs: map[string]string{},
			runs: map[string]*memory.Run{}, entities: map[string]*memory.Entity{}, byID: map[string]*memory.Entity{},
			aliases: map[string]string{}, links: map[string]map[string]bool{}, memEnts: map[string][]string{},
			vectors:       map[string]map[string]vector.Point{},
			relationships: map[string]relations.Link{},
			operations:    map[string]*store.Receipt{},
		}
		s.tenants[name] = t
	}
	return t
}

// Close releases nothing; it exists to satisfy store.Store.
func (s *Store) Close() error { return nil }

// Capabilities reports exact BM25.
func (s *Store) Capabilities() store.Capabilities { return store.Capabilities{BM25: "exact"} }

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{memory.ErrConflict}, a...)...)
}

// Write applies the batch atomically: all conditions are checked before any
// change is made, under one lock.
func (s *Store) Write(_ context.Context, b *store.Batch) (*store.Receipt, error) {
	if b.Tenant == "" {
		return nil, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.t(b.Tenant)
	now := s.now()
	if b.OperationKey != "" {
		if rec := t.operations[b.OperationKey]; rec != nil {
			if rec.OperationHash != b.OperationHash {
				return nil, conflict("operation hash changed")
			}
			out := cloneReceipt(rec)
			out.Replayed = true
			return out, nil
		}
	}
	if len(b.Result) != 0 && !json.Valid(b.Result) {
		return nil, fmt.Errorf("%w: invalid result JSON", memory.ErrInvalid)
	}
	if _, err := json.Marshal(b); err != nil {
		return nil, fmt.Errorf("%w: batch cannot be encoded: %v", memory.ErrInvalid, err)
	}
	if err := s.check(t, b); err != nil {
		return nil, err
	}
	existing := make([]relations.Link, 0, len(t.relationships))
	for _, r := range t.relationships {
		existing = append(existing, r)
	}
	present := map[string]bool{}
	for _, m := range b.Memories {
		present[m.ID] = true
	}
	links, jobs, err := store.PlanRelations(b, existing, func(id string) bool { return t.mems[id] != nil || present[id] }, now)
	if err != nil {
		return nil, err
	}

	for _, c := range b.Closes {
		m := t.mems[c.ID]
		if c.ValidTo != nil {
			v := *c.ValidTo
			m.ValidTo = &v
			at := now
			if c.ClosedAt != nil {
				at = *c.ClosedAt
			}
			m.ClosedAt = &at
		}
		if c.InvalidatedAt != nil {
			v := *c.InvalidatedAt
			m.InvalidatedAt = &v
		}
		if k := store.SlotKey(m); k != "" && t.slots[k] == m.ID {
			delete(t.slots, k)
		}
	}
	for i := range b.Memories {
		m := cloneMem(&b.Memories[i])
		m.Entities = nil
		t.mems[m.ID] = m
		if k := store.SlotKey(m); k != "" && store.Open(m) {
			t.slots[k] = m.ID
		}
		for _, ref := range b.Memories[i].Entities {
			e := t.resolve(ref)
			if t.links[e.ID] == nil {
				t.links[e.ID] = map[string]bool{}
			}
			if !t.links[e.ID][m.ID] {
				t.links[e.ID][m.ID] = true
				t.memEnts[m.ID] = append(t.memEnts[m.ID], e.ID)
				e.Mentions++
			}
		}
	}
	for _, tc := range b.Touches {
		if m := t.mems[tc.ID]; m != nil {
			m.Heat += tc.Heat
			m.Source.Evidence = append(m.Source.Evidence, tc.Evidence...)
		}
	}
	for _, id := range b.Deletes {
		t.deleteMem(id)
	}
	for _, e := range b.Events {
		t.history[e.MemoryID] = append(t.history[e.MemoryID], e)
	}

	rec := &store.Receipt{Result: append(json.RawMessage(nil), b.Result...), OperationHash: b.OperationHash}
	for _, ep := range b.Episodes {
		if ep.ClientMsgID != "" {
			if id, ok := t.clientIDs[ep.Run+"\x00"+ep.ClientMsgID]; ok {
				rec.Episodes = append(rec.Episodes, store.EpisodeResult{Episode: cloneEp(t.episodes[id]), Duplicate: true})
				continue
			}
		}
		r := t.runs[ep.Run]
		if r == nil {
			r = &memory.Run{Scope: memory.Scope{Tenant: b.Tenant, User: ep.User, Agent: ep.Agent, Run: ep.Run}}
			t.runs[ep.Run] = r
		}
		r.LastSeq++
		r.UpdatedAt = now
		e := cloneEp(&ep)
		e.Tenant, e.Seq, e.RecordedAt = b.Tenant, r.LastSeq, now
		if e.ID == "" {
			e.ID = memory.NewID()
		}
		if e.Status == "" {
			e.Status = memory.Final
		}
		t.episodes[e.ID] = &e
		t.runEps[e.Run] = append(t.runEps[e.Run], e.ID)
		if e.ClientMsgID != "" {
			t.clientIDs[e.Run+"\x00"+e.ClientMsgID] = e.ID
		}
		rec.Episodes = append(rec.Episodes, store.EpisodeResult{Episode: cloneEp(&e)})
	}
	for _, ed := range b.EpisodeEdits {
		if e := t.episodes[ed.ID]; e != nil {
			e.Status = ed.Status
			if ed.Parts != nil {
				e.Parts = append([]memory.Part(nil), ed.Parts...)
			}
		}
	}
	for _, run := range b.DeleteRuns {
		for _, id := range t.runEps[run] {
			e := t.episodes[id]
			delete(t.clientIDs, run+"\x00"+e.ClientMsgID)
			delete(t.episodes, id)
			t.deleteVectorsOf(vector.Target{Kind: vector.TargetPart, ID: id}, true)
		}
		delete(t.runEps, run)
		delete(t.runs, run)
	}
	for _, c := range b.Cursors {
		t.runs[c.Run].ProcessedSeq = c.To
	}
	for _, sm := range b.Summaries {
		if r := t.runs[sm.Run]; r != nil {
			r.Summary, r.SummaryUpto, r.UpdatedAt = sm.Text, sm.Upto, now
		}
	}
	for _, p := range b.Vectors {
		p.Tenant = b.Tenant
		p.Values = append([]float32(nil), p.Values...)
		_ = vector.Normalize(p.Values) // validated in check
		if t.vectors[p.Model] == nil {
			t.vectors[p.Model] = map[string]vector.Point{}
		}
		t.vectors[p.Model][p.Target.Key()] = p
		_ = s.vec.Upsert(context.Background(), []vector.Point{p})
	}
	for _, tg := range b.VectorDeletes {
		t.deleteVectorsOf(tg, false)
	}
	_ = s.vec.Delete(context.Background(), b.Tenant, b.VectorDeletes)
	for _, a := range b.Aliases {
		if t.entities[a.EntityKey] != nil {
			key := memory.EntityKey(a.Name)
			t.aliases[key] = a.EntityKey
			e := t.entities[a.EntityKey]
			e.Aliases = appendUnique(e.Aliases, a.Name)
		}
	}
	for _, j := range b.Jobs {
		s.enqueue(b.Tenant, j, now)
	}
	for _, j := range jobs {
		s.enqueue(b.Tenant, j, now)
	}
	t.relationships = map[string]relations.Link{}
	for _, r := range links {
		t.relationships[r.ID] = cloneLink(r)
	}
	if b.Settings != nil {
		t.settings = append([]byte(nil), b.Settings...)
	}
	for _, old := range t.operations {
		store.RedactReceipt(old, b.Deletes)
	}
	store.RedactReceipt(rec, b.Deletes)
	if b.OperationKey != "" {
		t.operations[b.OperationKey] = cloneReceipt(rec)
	}
	return rec, nil
}

// check validates every conditional change of b without mutating anything.
func (s *Store) check(t *tenant, b *store.Batch) error {
	for _, c := range b.Checks {
		m := t.mems[c.ID]
		if m == nil || m.Version != c.Version || (c.Open && !store.Open(m)) {
			return conflict("memory %s changed", c.ID)
		}
	}
	closing := map[string]bool{}
	for _, c := range b.Closes {
		m := t.mems[c.ID]
		if m == nil || !store.Open(m) || closing[c.ID] {
			return conflict("memory %s is not open", c.ID)
		}
		closing[c.ID] = true
	}
	deleting := map[string]bool{}
	for _, id := range b.Deletes {
		deleting[id] = true
	}
	taken := map[string]bool{}
	ids := map[string]bool{}
	for i := range b.Memories {
		m := &b.Memories[i]
		if m.ID == "" || t.mems[m.ID] != nil || ids[m.ID] {
			return fmt.Errorf("%w: memory id %q missing or already used", memory.ErrInvalid, m.ID)
		}
		ids[m.ID] = true
		k := store.SlotKey(m)
		if k == "" || !store.Open(m) {
			continue
		}
		if cur, ok := t.slots[k]; (ok && !closing[cur] && !deleting[cur]) || taken[k] {
			return conflict("slot %q already has an open value", m.Slot)
		}
		taken[k] = true
	}
	for _, p := range b.Vectors {
		if err := vector.Normalize(append([]float32(nil), p.Values...)); err != nil || p.Model == "" || p.Target.ID == "" {
			return fmt.Errorf("%w: bad vector for %s", memory.ErrInvalid, p.Target.Key())
		}
	}
	cursors := map[string]int64{}
	for run, r := range t.runs {
		cursors[run] = r.ProcessedSeq
	}
	episodeIDs := map[string]bool{}
	clientIDs := map[string]bool{}
	for _, ep := range b.Episodes {
		clientKey := ep.Run + "\x00" + ep.ClientMsgID
		if ep.ClientMsgID != "" && (t.clientIDs[clientKey] != "" || clientIDs[clientKey]) {
			continue
		}
		if ep.ID != "" && (t.episodes[ep.ID] != nil || episodeIDs[ep.ID]) {
			return conflict("episode %s already exists", ep.ID)
		}
		if ep.ID != "" {
			episodeIDs[ep.ID] = true
		}
		if ep.ClientMsgID != "" {
			clientIDs[clientKey] = true
		}
		if _, ok := cursors[ep.Run]; !ok {
			cursors[ep.Run] = 0
		}
	}
	for _, run := range b.DeleteRuns {
		delete(cursors, run)
	}
	for _, c := range b.Cursors {
		from, ok := cursors[c.Run]
		if !ok || from != c.From {
			return conflict("run %s cursor moved", c.Run)
		}
		cursors[c.Run] = c.To
	}
	pending := map[string]bool{}
	jobIDs := map[string]bool{}
	for id, j := range s.jobs {
		jobIDs[id] = true
		if j.Tenant == b.Tenant && j.State == store.JobPending && j.Key != "" {
			pending[j.Kind+"\x00"+j.Key] = true
		}
	}
	for _, j := range b.Jobs {
		key := j.Kind + "\x00" + j.Key
		if j.Key != "" && pending[key] {
			continue
		}
		if j.ID != "" && jobIDs[j.ID] {
			return conflict("job %s already exists", j.ID)
		}
		if j.ID != "" {
			jobIDs[j.ID] = true
		}
		if j.Key != "" {
			pending[key] = true
		}
	}
	return nil
}

func (t *tenant) resolve(ref memory.EntityRef) *memory.Entity {
	key := memory.EntityKey(ref.Name)
	if k, ok := t.aliases[key]; ok {
		key = k
	}
	e := t.entities[key]
	if e == nil {
		e = &memory.Entity{ID: memory.NewID(), Key: key, Name: ref.Name, Type: ref.Type}
		t.entities[key] = e
		t.byID[e.ID] = e
	}
	if e.Type == "" {
		e.Type = ref.Type
	}
	return e
}

func (t *tenant) deleteMem(id string) {
	for key, link := range t.relationships {
		if link.Source == id || link.Target == id {
			delete(t.relationships, key)
		}
	}
	m := t.mems[id]
	if m == nil {
		return
	}
	if k := store.SlotKey(m); k != "" && t.slots[k] == id {
		delete(t.slots, k)
	}
	for _, eid := range t.memEnts[id] {
		delete(t.links[eid], id)
		if e := t.byID[eid]; e != nil {
			e.Mentions--
		}
	}
	delete(t.memEnts, id)
	delete(t.mems, id)
	t.deleteVectorsOf(vector.Target{Kind: vector.TargetMemory, ID: id}, false)
	for i := range t.history[id] { // keep the audit trail, drop the content
		t.history[id][i].Old, t.history[id][i].New = "", ""
	}
}

func (t *tenant) deleteVectorsOf(tg vector.Target, allParts bool) {
	for _, pts := range t.vectors {
		if !allParts {
			delete(pts, tg.Key())
			continue
		}
		for k, p := range pts {
			if p.Target.Kind == vector.TargetPart && p.Target.ID == tg.ID {
				delete(pts, k)
			}
		}
	}
}

// Get returns copies of the requested memories with entities populated.
func (s *Store) Get(_ context.Context, tenant string, ids []string) ([]memory.Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	out := make([]memory.Memory, 0, len(ids))
	for _, id := range ids {
		if m := t.mems[id]; m != nil {
			out = append(out, t.withEntities(m))
		}
	}
	return out, nil
}

func (t *tenant) withEntities(m *memory.Memory) memory.Memory {
	c := *cloneMem(m)
	for _, eid := range t.memEnts[m.ID] {
		if e := t.byID[eid]; e != nil {
			c.Entities = append(c.Entities, memory.EntityRef{Type: e.Type, Name: e.Name})
		}
	}
	return c
}

// Find scans the tenant's memories.
func (s *Store) Find(_ context.Context, q store.Query) ([]memory.Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[q.Tenant]
	if t == nil {
		return nil, nil
	}
	now := s.now()
	var out []memory.Memory
	for _, m := range t.mems {
		if !q.Admits(m, now) {
			continue
		}
		c := t.withEntities(m)
		if q.Filter.Match(&c) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		switch q.Order {
		case store.OldestFirst:
			return out[i].ID < out[j].ID
		case store.HottestFirst:
			if out[i].Heat != out[j].Heat {
				return out[i].Heat > out[j].Heat
			}
		}
		return out[i].ID > out[j].ID
	})
	return page(out, q.Offset, q.Limit), nil
}

func page[T any](xs []T, offset, limit int) []T {
	if offset >= len(xs) {
		return nil
	}
	xs = xs[offset:]
	if limit > 0 && len(xs) > limit {
		xs = xs[:limit]
	}
	return xs
}

// History returns the events of one memory.
func (s *Store) History(_ context.Context, tenant, id string) ([]memory.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	return append([]memory.Event(nil), t.history[id]...), nil
}

// Episodes returns episodes matching q.
func (s *Store) Episodes(_ context.Context, q store.EpisodeQuery) ([]memory.Episode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.episodes(q), nil
}

func (s *Store) episodes(q store.EpisodeQuery) []memory.Episode {
	t := s.tenants[q.Tenant]
	if t == nil {
		return nil
	}
	var out []memory.Episode
	for _, e := range t.episodes {
		if matchEp(e, q) {
			out = append(out, cloneEp(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Run != b.Run {
			if q.Desc {
				return a.ID > b.ID
			}
			return a.ID < b.ID
		}
		if q.Desc {
			return a.Seq > b.Seq
		}
		return a.Seq < b.Seq
	})
	return page(out, 0, q.Limit)
}

func matchEp(e *memory.Episode, q store.EpisodeQuery) bool {
	if len(q.IDs) > 0 && !contains(q.IDs, e.ID) {
		return false
	}
	if (q.Run != "" && e.Run != q.Run) || (q.User != "" && e.User != q.User) || (q.Agent != "" && e.Agent != q.Agent) {
		return false
	}
	if len(q.Roles) > 0 && !contains(q.Roles, e.Role) {
		return false
	}
	if e.Seq <= q.AfterSeq || (q.BeforeSeq > 0 && e.Seq >= q.BeforeSeq) {
		return false
	}
	if (!q.Since.IsZero() && e.OccurredAt.Before(q.Since)) || (!q.Until.IsZero() && !e.OccurredAt.Before(q.Until)) {
		return false
	}
	if !q.IncludeDeleted && e.Status == memory.Deleted {
		return false
	}
	if q.BlobSHA != "" {
		for _, p := range e.Parts {
			if p.Blob != nil && p.Blob.SHA256 == q.BlobSHA {
				return true
			}
		}
		return false
	}
	return true
}

// Run returns run state.
func (s *Store) Run(_ context.Context, tenant, run string) (memory.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t := s.tenants[tenant]; t != nil && t.runs[run] != nil {
		return *t.runs[run], nil
	}
	return memory.Run{}, memory.ErrNotFound
}

// Settings returns the tenant settings document.
func (s *Store) Settings(_ context.Context, tenant string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t := s.tenants[tenant]; t != nil {
		return append([]byte(nil), t.settings...), nil
	}
	return nil, nil
}

// SearchMemories ranks memories with Okapi BM25 (k1=1.2, b=0.75) using
// statistics of all of the tenant's memories.
func (s *Store) SearchMemories(_ context.Context, q store.TextQuery) ([]store.Hit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[q.Tenant]
	if t == nil || len(q.Terms) == 0 {
		return nil, nil
	}
	now := s.now()
	docs := make(map[string][]string, len(t.mems))
	for id, m := range t.mems {
		docs[id] = textutil.Tokens(m.Title + " " + m.Text)
	}
	scores := bm25(docs, q.Terms)
	var out []store.Hit
	for id, sc := range scores {
		m := t.withEntities(t.mems[id])
		if q.Admits(&m, now) && q.Filter.Match(&m) {
			out = append(out, store.Hit{ID: id, Score: sc})
		}
	}
	sortHits(out)
	return page(out, q.Offset, q.Limit), nil
}

// SearchEpisodes ranks episodes with BM25.
func (s *Store) SearchEpisodes(_ context.Context, q store.EpisodeTextQuery) ([]store.EpisodeHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[q.Tenant]
	if t == nil || len(q.Terms) == 0 {
		return nil, nil
	}
	docs := make(map[string][]string, len(t.episodes))
	for id, e := range t.episodes {
		docs[id] = textutil.Tokens(e.Text())
	}
	var out []store.EpisodeHit
	for id, sc := range bm25(docs, q.Terms) {
		if e := t.episodes[id]; matchEp(e, q.EpisodeQuery) {
			out = append(out, store.EpisodeHit{Episode: cloneEp(e), Score: sc})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Episode.ID > out[j].Episode.ID
	})
	return page(out, 0, q.Limit), nil
}

func sortHits(h []store.Hit) {
	sort.Slice(h, func(i, j int) bool {
		if h[i].Score != h[j].Score {
			return h[i].Score > h[j].Score
		}
		return h[i].ID > h[j].ID
	})
}

// bm25 scores documents containing at least one term.
func bm25(docs map[string][]string, terms []string) map[string]float64 {
	const k1, b = 1.2, 0.75
	n := float64(len(docs))
	var total float64
	df := map[string]float64{}
	tf := map[string]map[string]float64{}
	for id, toks := range docs {
		total += float64(len(toks))
		seen := map[string]bool{}
		for _, tok := range toks {
			for _, term := range terms {
				if tok == term || (utf8.RuneCountInString(term) >= store.PrefixMin && strings.HasPrefix(tok, term)) {
					if tf[id] == nil {
						tf[id] = map[string]float64{}
					}
					tf[id][term]++
					if !seen[term] {
						seen[term] = true
						df[term]++
					}
				}
			}
		}
	}
	avg := total / math.Max(n, 1)
	out := map[string]float64{}
	for id, freqs := range tf {
		dl := float64(len(docs[id]))
		var sc float64
		for term, f := range freqs {
			idf := math.Log(1 + (n-df[term]+0.5)/(df[term]+0.5))
			sc += idf * f * (k1 + 1) / (f + k1*(1-b+b*dl/math.Max(avg, 1)))
		}
		out[id] = sc
	}
	return out
}

// FindEntities matches keys and alias keys.
func (s *Store) FindEntities(_ context.Context, tenant string, keys []string) ([]memory.Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	var out []memory.Entity
	seen := map[string]bool{}
	for _, k := range keys {
		if a, ok := t.aliases[k]; ok {
			k = a
		}
		if e := t.entities[k]; e != nil && !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, cloneEntity(e))
		}
	}
	return out, nil
}

// EntityMemories returns up to limit links per entity, newest memory first.
func (s *Store) EntityMemories(_ context.Context, tenant string, ids []string, limit int) ([]store.Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	var out []store.Link
	for _, eid := range ids {
		var mids []string
		for mid := range t.links[eid] {
			mids = append(mids, mid)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(mids)))
		for _, mid := range page(mids, 0, limit) {
			out = append(out, store.Link{EntityID: eid, MemoryID: mid})
		}
	}
	return out, nil
}

// ListEntities lists entities by key prefix, most mentioned first.
func (s *Store) ListEntities(_ context.Context, tenant, prefix string, limit int) ([]memory.Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	var out []memory.Entity
	for k, e := range t.entities {
		if strings.HasPrefix(k, prefix) {
			out = append(out, cloneEntity(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mentions != out[j].Mentions {
			return out[i].Mentions > out[j].Mentions
		}
		return out[i].Key < out[j].Key
	})
	return page(out, 0, limit), nil
}

// Upsert implements vector.Index for points written outside a Batch.
func (s *Store) Upsert(ctx context.Context, points []vector.Point) error {
	byTenant := map[string][]vector.Point{}
	for _, p := range points {
		byTenant[p.Tenant] = append(byTenant[p.Tenant], p)
	}
	for tn, pts := range byTenant {
		if _, err := s.Write(ctx, &store.Batch{Tenant: tn, Vectors: pts}); err != nil {
			return err
		}
	}
	return nil
}

// Delete implements vector.Index.
func (s *Store) Delete(ctx context.Context, tenant string, targets []vector.Target) error {
	_, err := s.Write(ctx, &store.Batch{Tenant: tenant, VectorDeletes: targets})
	return err
}

// Search implements vector.Index with an exact scan.
func (s *Store) Search(ctx context.Context, q vector.Query) ([]vector.Hit, error) {
	return s.vec.Search(ctx, q)
}

// Vectors pages through stored points of a model.
func (s *Store) Vectors(_ context.Context, tenant, model, after string, limit int) ([]vector.Point, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	var keys []string
	for k := range t.vectors[model] {
		if k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []vector.Point
	for _, k := range page(keys, 0, limit) {
		out = append(out, t.vectors[model][k])
	}
	return out, nil
}

// Missing lists current memories without a vector for model, oldest first.
func (s *Store) Missing(_ context.Context, tenant, model string, limit int) ([]vector.Target, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	var ids []string
	for id, m := range t.mems {
		tg := vector.Target{Kind: vector.TargetMemory, ID: id}
		if _, ok := t.vectors[model][tg.Key()]; !ok && store.Open(m) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var out []vector.Target
	for _, id := range page(ids, 0, limit) {
		out = append(out, vector.Target{Kind: vector.TargetMemory, ID: id})
	}
	return out, nil
}

// Coverage counts open memories with and without a vector.
func (s *Store) Coverage(_ context.Context, tenant, model string) (int, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return 0, 0, nil
	}
	have, total := 0, 0
	for id, m := range t.mems {
		if !store.Open(m) {
			continue
		}
		total++
		if _, ok := t.vectors[model][vector.Target{Kind: vector.TargetMemory, ID: id}.Key()]; ok {
			have++
		}
	}
	return have, total, nil
}

// Tenants lists tenants with record counts.
func (s *Store) Tenants(_ context.Context) ([]store.TenantStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.TenantStats
	for name, t := range s.tenants {
		st := store.TenantStats{Tenant: name, Memories: len(t.mems), Episodes: len(t.episodes), Entities: len(t.entities)}
		for _, j := range s.jobs {
			if j.Tenant == name {
				st.Jobs++
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out, nil
}

func (s *Store) enqueue(tenant string, j store.Job, now time.Time) {
	if j.NotBefore.IsZero() {
		j.NotBefore = now
	}
	if j.Key != "" {
		for _, p := range s.jobs {
			if p.Tenant == tenant && p.Kind == j.Kind && p.Key == j.Key && p.State == store.JobPending {
				if j.NotBefore.After(p.NotBefore) {
					p.NotBefore = j.NotBefore
				}
				p.UpdatedAt = now
				return
			}
		}
	}
	if j.ID == "" {
		j.ID = memory.NewID()
	}
	j.Tenant, j.State, j.CreatedAt, j.UpdatedAt = tenant, store.JobPending, now, now
	s.jobs[j.ID] = &j
}

// Claim leases due jobs, round-robin across tenants. Pending jobs and jobs
// whose lease expired (crashed workers) are both claimable.
func (s *Store) Claim(_ context.Context, worker string, n int, lease time.Duration) ([]store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	busy := map[string]bool{}
	for _, j := range s.jobs {
		if j.State == store.JobLeased && j.LeaseUntil.After(now) && j.Key != "" {
			busy[j.Tenant+"\x00"+j.Kind+"\x00"+j.Key] = true
		}
	}
	perTenant := map[string][]*store.Job{}
	for _, j := range s.jobs {
		due := (j.State == store.JobPending && !j.NotBefore.After(now)) ||
			(j.State == store.JobLeased && !j.LeaseUntil.After(now))
		if due && !busy[j.Tenant+"\x00"+j.Kind+"\x00"+j.Key] {
			perTenant[j.Tenant] = append(perTenant[j.Tenant], j)
		}
	}
	tenants := make([]string, 0, len(perTenant))
	for tn, js := range perTenant {
		sort.Slice(js, func(a, b int) bool {
			if !js[a].NotBefore.Equal(js[b].NotBefore) {
				return js[a].NotBefore.Before(js[b].NotBefore)
			}
			return js[a].ID < js[b].ID
		})
		tenants = append(tenants, tn)
	}
	sort.Strings(tenants)
	var out []store.Job
	for round := 0; len(out) < n; round++ {
		took := false
		for _, tn := range tenants {
			js := perTenant[tn]
			if round >= len(js) || len(out) == n {
				continue
			}
			j := js[round]
			k := j.Tenant + "\x00" + j.Kind + "\x00" + j.Key
			if j.Key != "" {
				if busy[k] {
					continue
				}
				busy[k] = true
			}
			j.State, j.Worker, j.LeaseUntil, j.UpdatedAt = store.JobLeased, worker, now.Add(lease), now
			j.Attempts++
			out = append(out, *j)
			took = true
		}
		if !took {
			break
		}
	}
	return out, nil
}

func (s *Store) leased(id, worker string) (*store.Job, error) {
	j := s.jobs[id]
	if j == nil || j.State != store.JobLeased || j.Worker != worker {
		return nil, conflict("job %s is not leased by %s", id, worker)
	}
	return j, nil
}

// Heartbeat extends a lease and saves a checkpoint.
func (s *Store) Heartbeat(_ context.Context, id, worker string, lease time.Duration, cp json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, worker)
	if err != nil {
		return err
	}
	now := s.now()
	j.LeaseUntil, j.UpdatedAt = now.Add(lease), now
	if cp != nil {
		j.Checkpoint = append(json.RawMessage(nil), cp...)
	}
	return nil
}

// Finish completes, retries or kills a job.
func (s *Store) Finish(_ context.Context, id, worker string, r store.Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, worker)
	if err != nil {
		return err
	}
	switch {
	case r.Done:
		delete(s.jobs, id)
	case r.Dead:
		j.State, j.LastError, j.Worker, j.UpdatedAt = store.JobDead, r.Error, "", s.now()
	default:
		// Merge a pending duplicate (same key, enqueued while this job ran).
		notBefore := r.RetryAt
		for oid, o := range s.jobs {
			if oid != id && j.Key != "" && o.State == store.JobPending && o.Tenant == j.Tenant && o.Kind == j.Kind && o.Key == j.Key {
				if o.NotBefore.After(notBefore) {
					notBefore = o.NotBefore
				}
				delete(s.jobs, oid)
			}
		}
		j.State, j.LastError, j.Worker, j.NotBefore, j.UpdatedAt = store.JobPending, r.Error, "", notBefore, s.now()
	}
	return nil
}

// Jobs lists jobs, oldest first.
func (s *Store) Jobs(_ context.Context, q store.JobQuery) ([]store.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.Job
	for _, j := range s.jobs {
		if (q.Tenant == "" || j.Tenant == q.Tenant) && (q.State == "" || j.State == q.State) && (q.Kind == "" || j.Kind == q.Kind) && (q.Key == "" || j.Key == q.Key) {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return page(out, 0, q.Limit), nil
}

func cloneMem(m *memory.Memory) *memory.Memory {
	c := *m
	c.Tags = append([]string(nil), m.Tags...)
	c.Grants = append([]string(nil), m.Grants...)
	c.Entities = append([]memory.EntityRef(nil), m.Entities...)
	c.Source.Evidence = append([]memory.Evidence(nil), m.Source.Evidence...)
	c.Source.Flags = append([]string(nil), m.Source.Flags...)
	if m.Metadata != nil {
		c.Metadata = make(map[string]any, len(m.Metadata))
		for k, v := range m.Metadata {
			c.Metadata[k] = v
		}
	}
	for _, p := range []**time.Time{&c.ValidTo, &c.InvalidatedAt, &c.ExpiresAt, &c.ClosedAt} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return &c
}

func cloneEp(e *memory.Episode) memory.Episode {
	c := *e
	c.Parts = append([]memory.Part(nil), e.Parts...)
	if e.Metadata != nil {
		c.Metadata = make(map[string]any, len(e.Metadata))
		for k, v := range e.Metadata {
			c.Metadata[k] = v
		}
	}
	return c
}

func cloneEntity(e *memory.Entity) memory.Entity {
	c := *e
	c.Aliases = append([]string(nil), e.Aliases...)
	return c
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func appendUnique(xs []string, s string) []string {
	if contains(xs, s) {
		return xs
	}
	return append(xs, s)
}
