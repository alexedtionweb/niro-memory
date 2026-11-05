// Package search is the retrieval engine. It never calls an LLM.
//
// A rule-table planner picks the cheapest strategies that can answer a
// request, runs them concurrently, fuses their scores, re-checks every
// candidate against the store (tenant, visibility, filters, validity) and
// explains each result:
//
//	IDs given            → exact lookup
//	no text, no vector   → filtered listing (newest first)
//	text                 → lexical (BM25) + entity lookup (+ graph hops)
//	vector, or text with an embedder that answers within the policy budget
//	                     → + vector similarity
//
// Candidates come from the union of all strategies, so a keyword-only or
// entity-only match is never lost (unlike vector-first designs).
package search

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-memory/cache"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/textutil"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Searcher runs retrieval requests. All fields except Store are optional.
type Searcher struct {
	Store    store.Store
	Index    vector.Index
	Embedder embed.Embedder
	Cache    cache.Cache
	// RulesFor provides entity patterns used to spot entities in queries.
	RulesFor func(ctx context.Context, tenant string) (*rules.Set, error)
	Now      func() time.Time
}

// QueryVector is a caller-supplied query embedding.
type QueryVector struct {
	Model  string    `json:"model"`
	Values []float32 `json:"values"`
}

// Request is one retrieval.
type Request struct {
	Tenant string `json:"-"`
	// As is the principal whose visibility applies. Zero means the owner
	// given by User and Agent.
	As               memory.Principal `json:"-"`
	User             string           `json:"user_id,omitempty"`
	Agent            string           `json:"agent_id,omitempty"`
	Run              string           `json:"run_id,omitempty"`
	Text             string           `json:"query,omitempty"`
	Vector           *QueryVector     `json:"vector,omitempty"`
	IDs              []string         `json:"ids,omitempty"`
	Filter           memory.Filter    `json:"-"`
	ValidAt, KnownAt time.Time        `json:"-"`
	AllVersions      bool             `json:"all_versions,omitempty"`
	ShowExpired      bool             `json:"show_expired,omitempty"`
	Limit            int              `json:"limit,omitempty"`
	Threshold        float64          `json:"threshold,omitempty"` // overrides the policy when > 0
	Policy           Policy           `json:"-"`
	Explain          bool             `json:"explain,omitempty"`
	// Model is the tenant's active embedding model, used to embed Text.
	Model string `json:"-"`
}

// Result is the answer to a Request.
type Result struct {
	Hits  []Hit         `json:"hits"`
	Trace []Step        `json:"trace"`
	Took  time.Duration `json:"took_ns"`
}

// Step reports what one strategy did; the planner trace explains cost.
type Step struct {
	Strategy   string        `json:"strategy"`
	Ran        bool          `json:"ran"`
	Reason     string        `json:"reason,omitempty"`
	Candidates int           `json:"candidates"`
	Took       time.Duration `json:"took_ns"`
}

// Hit is one result with its score and, on request, its explanation.
type Hit struct {
	Memory  memory.Memory `json:"memory"`
	Score   float64       `json:"score"`
	Explain *Explain      `json:"explain,omitempty"`
}

// Explain breaks a score into its components so agents and developers can
// see why a memory was retrieved.
type Explain struct {
	Found         []string       `json:"found_by"`
	Lexical       float64        `json:"lexical"`
	Vector        float64        `json:"vector"`
	Entity        float64        `json:"entity"`
	Entities      []string       `json:"entities,omitempty"`
	Relevance     float64        `json:"relevance"`
	Recency       float64        `json:"recency"`
	Confidence    float64        `json:"confidence"`
	Importance    float64        `json:"importance"`
	ContentWeight float64        `json:"content_weight"`
	Prior         float64        `json:"prior"`
	Final         float64        `json:"final"`
	Ranks         map[string]int `json:"ranks,omitempty"`
}

const (
	maxLimit      = 100
	minCandidates = 60
	maxTerms      = 32
)

// cand accumulates the signals of one candidate memory.
type cand struct {
	id       string
	lexRaw   float64
	vec      float64
	ent      float64
	entities []string
	found    []string
	ranks    map[string]int
}

type collector struct {
	mu    sync.Mutex
	cands map[string]*cand
	steps []Step
}

func (c *collector) get(id string) *cand {
	x := c.cands[id]
	if x == nil {
		x = &cand{id: id, ranks: map[string]int{}}
		c.cands[id] = x
	}
	return x
}

func (c *collector) step(s Step) {
	c.mu.Lock()
	c.steps = append(c.steps, s)
	c.mu.Unlock()
}

// Search runs a request.
func (s *Searcher) Search(ctx context.Context, r Request) (*Result, error) {
	start := time.Now()
	if r.Tenant == "" {
		return nil, fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	if r.Limit <= 0 {
		r.Limit = 10
	}
	if r.Limit > maxLimit {
		r.Limit = maxLimit
	}
	if r.Policy.Name == "" {
		r.Policy = Default["default"]
	}
	if r.Threshold <= 0 {
		r.Threshold = r.Policy.Threshold
	}
	if r.As.User == "" && r.As.Agent == "" && len(r.As.Roles) == 0 && !r.As.Admin {
		r.As = memory.Principal{User: r.User, Agent: r.Agent}
	}
	f := memory.And(memory.Readable(r.As), r.Filter)
	for field, v := range map[string]string{"user_id": r.User, "agent_id": r.Agent, "run_id": r.Run} {
		if v != "" {
			f.And = append(f.And, memory.Eq(field, v))
		}
	}
	if err := f.Check(); err != nil {
		return nil, err
	}
	q := store.Query{Tenant: r.Tenant, Filter: f, ValidAt: r.ValidAt, KnownAt: r.KnownAt, AllVersions: r.AllVersions, ShowExpired: r.ShowExpired}
	now := s.now()
	var res *Result
	var err error
	switch {
	case len(r.IDs) > 0:
		res, err = s.exact(ctx, r, q, now)
	case r.Text == "" && r.Vector == nil:
		res, err = s.list(ctx, r, q, now)
	default:
		res, err = s.hybrid(ctx, r, q, now)
	}
	if res != nil {
		if res.Hits == nil {
			res.Hits = []Hit{}
		}
		if res.Trace == nil {
			res.Trace = []Step{}
		}
		res.Took = time.Since(start)
	}
	return res, err
}

func (s *Searcher) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Searcher) exact(ctx context.Context, r Request, q store.Query, now time.Time) (*Result, error) {
	t := time.Now()
	ms, err := s.Store.Get(ctx, r.Tenant, r.IDs)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for i := range ms {
		if q.Filter.Match(&ms[i]) && q.Admits(&ms[i], now) {
			res.Hits = append(res.Hits, s.hit(ms[i], &cand{found: []string{"exact"}}, 1, nil, r, now))
		}
	}
	res.Trace = []Step{{Strategy: "exact", Ran: true, Candidates: len(ms), Took: time.Since(t)}}
	return res, nil
}

func (s *Searcher) list(ctx context.Context, r Request, q store.Query, now time.Time) (*Result, error) {
	t := time.Now()
	q.Limit = r.Limit
	ms, err := s.Store.Find(ctx, q)
	if err != nil {
		return nil, err
	}
	res := &Result{Trace: []Step{{Strategy: "filter", Ran: true, Reason: "no query text or vector", Candidates: len(ms), Took: time.Since(t)}}}
	for _, m := range ms {
		res.Hits = append(res.Hits, s.hit(m, &cand{found: []string{"filter"}}, 1, nil, r, now))
	}
	return res, nil
}

func (s *Searcher) hybrid(ctx context.Context, r Request, q store.Query, now time.Time) (*Result, error) {
	n := r.Limit * max(r.Policy.Overfetch, 1)
	if n < minCandidates {
		n = minCandidates
	}
	terms := textutil.Terms(r.Text, maxTerms)
	c := &collector{cands: map[string]*cand{}}
	var wg sync.WaitGroup
	var errs []error
	var emu sync.Mutex
	fail := func(strategy string, err error) {
		emu.Lock()
		errs = append(errs, fmt.Errorf("%s: %w", strategy, err))
		emu.Unlock()
		c.step(Step{Strategy: strategy, Ran: true, Reason: "error: " + err.Error()})
	}
	active := map[string]bool{}
	var amu sync.Mutex
	mark := func(s string) { amu.Lock(); active[s] = true; amu.Unlock() }

	lex, hasLex := s.Store.(store.Lexical)
	switch {
	case r.Text == "":
		c.step(Step{Strategy: "lexical", Reason: "no query text"})
	case !hasLex:
		c.step(Step{Strategy: "lexical", Reason: "store has no full-text search"})
	case len(terms) == 0:
		c.step(Step{Strategy: "lexical", Reason: "no searchable terms"})
	default:
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			lq := q
			lq.Limit = n
			hits, err := lex.SearchMemories(ctx, store.TextQuery{Query: lq, Terms: terms})
			if err != nil {
				fail("lexical", err)
				return
			}
			c.mu.Lock()
			for i, h := range hits {
				x := c.get(h.ID)
				x.lexRaw, x.found, x.ranks["lexical"] = h.Score, append(x.found, "lexical"), i+1
			}
			c.mu.Unlock()
			if len(hits) > 0 {
				mark("lexical")
			}
			c.step(Step{Strategy: "lexical", Ran: true, Candidates: len(hits), Took: time.Since(t)})
		}()
	}

	ents, hasEnt := s.Store.(store.Entities)
	switch {
	case r.Text == "":
		c.step(Step{Strategy: "entity", Reason: "no query text"})
	case !hasEnt:
		c.step(Step{Strategy: "entity", Reason: "store has no entity index"})
	case r.Policy.GraphHops == 0 && r.Policy.Entity == 0:
		c.step(Step{Strategy: "entity", Reason: "disabled by policy"})
	default:
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			found, err := s.entities(ctx, r, ents, n)
			if err != nil {
				fail("entity", err)
				return
			}
			c.mu.Lock()
			ids := make([]string, 0, len(found))
			for id := range found {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool {
				if found[ids[i]].boost != found[ids[j]].boost {
					return found[ids[i]].boost > found[ids[j]].boost
				}
				return ids[i] > ids[j]
			})
			for rank, id := range ids {
				e := found[id]
				x := c.get(id)
				if e.boost > x.ent {
					x.ent = e.boost
				}
				x.entities, x.found, x.ranks["entity"] = append(x.entities, e.names...), append(x.found, "entity"), rank+1
			}
			c.mu.Unlock()
			if len(found) > 0 {
				mark("entity")
			}
			c.step(Step{Strategy: "entity", Ran: true, Candidates: len(found), Took: time.Since(t)})
		}()
	}

	if reason := s.vectorPlan(r); reason != "" {
		c.step(Step{Strategy: "vector", Reason: reason})
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			qv, model, why := s.queryVector(ctx, r)
			if qv == nil {
				c.step(Step{Strategy: "vector", Reason: why, Took: time.Since(t)})
				return
			}
			hits, err := s.Index.Search(ctx, vector.Query{Tenant: r.Tenant, Model: model, Values: qv, K: n,
				User: r.User, Agent: r.Agent, Run: r.Run, Kind: vector.TargetMemory})
			if err != nil {
				fail("vector", err)
				return
			}
			c.mu.Lock()
			for i, h := range hits {
				x := c.get(h.Target.ID)
				x.vec, x.found, x.ranks["vector"] = math.Max(0, h.Score), append(x.found, "vector"), i+1
			}
			c.mu.Unlock()
			if len(hits) > 0 {
				mark("vector")
			}
			c.step(Step{Strategy: "vector", Ran: true, Reason: why, Candidates: len(hits), Took: time.Since(t)})
		}()
	}
	wg.Wait()
	if len(errs) > 0 && len(c.cands) == 0 {
		return nil, errors.Join(errs...)
	}
	if r.Policy.RelationshipHops != 0 {
		t := time.Now()
		count, err := s.relationshipCandidates(ctx, r, q, now, c, min(n, 400))
		if err != nil {
			return nil, err
		}
		if count > 0 {
			active["entity"] = true
		}
		c.step(Step{Strategy: "relationships", Ran: true, Candidates: count, Took: time.Since(t)})
	}
	return s.rank(ctx, r, q, now, c, active, len(terms))
}

// lexicalNorm returns how raw lexical scores become [0,1]: exact BM25
// scores go through a query-length-adaptive sigmoid; other backends' scores
// (e.g. PostgreSQL ts_rank) are divided by the best score of the query.
func (s *Searcher) lexicalNorm(c *collector, nTerms int) func(raw float64) float64 {
	if d, ok := s.Store.(store.Describer); !ok || d.Capabilities().BM25 == "exact" {
		return func(raw float64) float64 { return bm25Sigmoid(raw, nTerms) }
	}
	best := 0.0
	for _, x := range c.cands {
		best = math.Max(best, x.lexRaw)
	}
	return func(raw float64) float64 {
		if best <= 0 {
			return 0
		}
		return raw / best
	}
}

type entityHit struct {
	boost float64
	names []string
}

// entities finds entities mentioned in the query (by n-gram key lookup and
// rule patterns) and the memories linked to them, optionally expanding one
// more hop through co-mentioned entities.
func (s *Searcher) entities(ctx context.Context, r Request, es store.Entities, n int) (map[string]entityHit, error) {
	keys := textutil.NGrams(textutil.Tokens(r.Text), 4)
	if len(keys) > 64 {
		keys = keys[:64]
	}
	if s.RulesFor != nil {
		if rs, err := s.RulesFor(ctx, r.Tenant); err == nil && rs != nil {
			for _, e := range rs.Entities(r.Text) {
				keys = append(keys, memory.EntityKey(e.Name))
			}
		}
	}
	found, err := es.FindEntities(ctx, r.Tenant, keys)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	out := map[string]entityHit{}
	seen := map[string]bool{}
	add := func(list []memory.Entity, factor float64) error {
		ids := make([]string, 0, len(list))
		byID := map[string]memory.Entity{}
		for _, e := range list {
			if !seen[e.ID] {
				seen[e.ID] = true
				ids = append(ids, e.ID)
				byID[e.ID] = e
			}
		}
		links, err := es.EntityMemories(ctx, r.Tenant, ids, n)
		if err != nil {
			return err
		}
		for _, l := range links {
			e := byID[l.EntityID]
			b := factor * entityBoost(1, int(e.Mentions))
			h := out[l.MemoryID]
			if b > h.boost {
				h.boost = b
			}
			h.names = append(h.names, e.Name)
			out[l.MemoryID] = h
		}
		return nil
	}
	if err := add(found, 1); err != nil {
		return nil, err
	}
	if r.Policy.GraphHops >= 2 && len(out) > 0 {
		ids := make([]string, 0, len(out))
		for id := range out {
			ids = append(ids, id)
		}
		ms, err := s.Store.Get(ctx, r.Tenant, ids)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, m := range ms {
			for _, e := range m.Entities {
				next = append(next, memory.EntityKey(e.Name))
			}
		}
		hop, err := es.FindEntities(ctx, r.Tenant, next)
		if err != nil {
			return nil, err
		}
		if err := add(hop, 0.5); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// vectorPlan returns why vector search is skipped, or "" to run it.
func (s *Searcher) vectorPlan(r Request) string {
	switch {
	case s.Index == nil:
		return "no vector index configured"
	case r.Vector != nil:
		return ""
	case s.Embedder == nil:
		return "no query vector and no embedder"
	case r.Policy.EmbedBudget <= 0:
		return "policy does not embed query text"
	case r.Model == "" || r.Model != s.Embedder.Model().ID:
		return "embedder model is not the tenant's active model"
	}
	return ""
}

// queryVector returns the request vector, or embeds the text within the
// policy budget. A slow embedding keeps running in the background and fills
// the cache for the next request, but this request does not wait for it.
func (s *Searcher) queryVector(ctx context.Context, r Request) ([]float32, string, string) {
	if r.Vector != nil {
		return r.Vector.Values, r.Vector.Model, "caller vector"
	}
	model := s.Embedder.Model().ID
	sum := sha256.Sum256([]byte(r.Text))
	key := "qemb:" + model + ":" + hex.EncodeToString(sum[:16])
	if s.Cache != nil {
		if b, ok := s.Cache.Get(ctx, key); ok {
			return decode(b), model, "query embedding cached"
		}
	}
	ch := make(chan []float32, 1)
	go func() {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), max(10*time.Second, r.Policy.EmbedBudget))
		defer cancel()
		vs, _, err := s.Embedder.Embed(ectx, embed.Query, []embed.Input{{Modality: "text", Text: r.Text}})
		if err != nil || len(vs) != 1 {
			ch <- nil
			return
		}
		if s.Cache != nil {
			s.Cache.Set(ectx, key, encode(vs[0]), time.Hour)
		}
		ch <- vs[0]
	}()
	timer := time.NewTimer(r.Policy.EmbedBudget)
	defer timer.Stop()
	select {
	case v := <-ch:
		if v == nil {
			return nil, "", "embedding failed"
		}
		return v, model, "query embedded"
	case <-timer.C:
		return nil, "", fmt.Sprintf("skipped: embedding exceeded budget %s", r.Policy.EmbedBudget)
	case <-ctx.Done():
		return nil, "", "cancelled"
	}
}

func encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decode(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// rank rehydrates candidates, re-checks them, scores, applies the
// threshold and diversifies.
func (s *Searcher) rank(ctx context.Context, r Request, q store.Query, now time.Time, c *collector, active map[string]bool, nTerms int) (*Result, error) {
	ids := make([]string, 0, len(c.cands))
	for id := range c.cands {
		ids = append(ids, id)
	}
	t := time.Now()
	ms, err := s.Store.Get(ctx, r.Tenant, ids)
	if err != nil {
		return nil, err
	}
	p := r.Policy
	norm := s.lexicalNorm(c, nTerms)
	weights := map[string]float64{"lexical": p.Lexical, "vector": p.Vector, "entity": p.Entity}
	var denom float64
	for st := range active {
		denom += weights[st]
	}
	var hits []Hit
	for i := range ms {
		m := ms[i]
		// Hits from indexes outside the store's query (vector, entity) are
		// re-checked here, so a stale index can never leak or resurrect data.
		if !q.Filter.Match(&m) || !q.Admits(&m, now) {
			continue
		}
		x := c.cands[m.ID]
		var rel float64
		if p.Fusion == "rrf" {
			k := p.RRFK
			if k <= 0 {
				k = 60
			}
			for _, rk := range x.ranks {
				rel += 1 / (k + float64(rk))
			}
			rel /= float64(max(len(active), 1)) / (k + 1)
		} else if denom > 0 {
			lexN := 0.0
			if x.lexRaw > 0 {
				lexN = norm(x.lexRaw)
			}
			rel = (p.Lexical*lexN + p.Vector*x.vec + p.Entity*x.ent) / denom
		}
		h := s.hit(m, x, math.Min(rel, 1), norm, r, now)
		if h.Score >= r.Threshold {
			hits = append(hits, h)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Memory.ID > hits[j].Memory.ID
	})
	redundant := 0
	if !r.AllVersions && r.KnownAt.IsZero() {
		hits, redundant = collapseDerivedPassages(hits)
	}
	hits = mmr(hits, r.Limit, p.MMRLambda)
	c.steps = append(c.steps, Step{Strategy: "rank", Ran: true, Candidates: len(ms), Took: time.Since(t),
		Reason: fmt.Sprintf("fusion=%s threshold=%.2f mmr=%.2f redundant_passages=%d; scores are ranking signals, not confidence", fusionName(p), r.Threshold, p.MMRLambda, redundant)})
	return &Result{Hits: hits, Trace: orderSteps(c.steps)}, nil
}

func fusionName(p Policy) string {
	if p.Fusion == "" {
		return "additive"
	}
	return p.Fusion
}

// hit combines relevance with the prior (recency, confidence, importance).
func (s *Searcher) hit(m memory.Memory, x *cand, rel float64, norm func(float64) float64, r Request, now time.Time) Hit {
	p := r.Policy
	if m.Source.Kind == "document" {
		p.Recency = 0 // Import time is not the age or relevance of a document's content.
	}
	rec := recency(now.Sub(m.ValidFrom), p.RecencyHalfLife)
	pw := p.Recency + p.Confidence + p.Importance
	prior := 0.0
	if pw > 0 {
		prior = (p.Recency*rec + p.Confidence*m.Confidence + p.Importance*m.Importance) / pw
	}
	contentWeight := 1.0
	if m.Source.Kind == "document" && m.Metadata["navigation_only"] == true {
		contentWeight = .5
	}
	final := ((1-p.PriorShare)*rel + p.PriorShare*prior) * contentWeight
	h := Hit{Memory: m, Score: final}
	if r.Explain {
		lexN := 0.0
		if x.lexRaw > 0 && norm != nil {
			lexN = norm(x.lexRaw)
		}
		h.Explain = &Explain{Found: x.found, Lexical: lexN, Vector: x.vec, Entity: x.ent, Entities: x.entities,
			Relevance: rel, Recency: rec, Confidence: m.Confidence, Importance: m.Importance,
			Prior: prior, Final: final, Ranks: x.ranks, ContentWeight: contentWeight}
	}
	return h
}

// Collapse only plain extractive copies whose original also passed authorization
// and the query filter. Structured assertions and historical lookups remain intact.
func collapseDerivedPassages(hits []Hit) ([]Hit, int) {
	originals := make(map[string]string, len(hits))
	for _, h := range hits {
		originals[h.Memory.ID] = memory.Normalize(h.Memory.Text)
	}
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		m := h.Memory
		if m.Source.Kind == "knowledge_enriched" && m.Subject == "" && m.Predicate == "" && m.Object == "" {
			if meta, ok := m.Metadata["enrichment"].(map[string]any); ok && meta["mode"] == "extractive" {
				if id, ok := meta["source_id"].(string); ok && id != m.ID && originals[id] != "" &&
					strings.Contains(originals[id], memory.Normalize(m.Text)) {
					continue
				}
			}
		}
		out = append(out, h)
	}
	return out, len(hits) - len(out)
}

// mmr greedily picks results that are relevant but not redundant
// (Carbonell & Goldstein 1998), using term-set Jaccard similarity.
func mmr(hits []Hit, k int, lambda float64) []Hit {
	if lambda >= 1 || len(hits) <= 1 {
		if len(hits) > k {
			hits = hits[:k]
		}
		return hits
	}
	terms := make([][]string, len(hits))
	for i, h := range hits {
		terms[i] = textutil.Terms(h.Memory.Text, 0)
	}
	picked := []int{}
	used := make([]bool, len(hits))
	for len(picked) < k && len(picked) < len(hits) {
		best, bestScore := -1, math.Inf(-1)
		for i := range hits {
			if used[i] {
				continue
			}
			maxSim := 0.0
			for _, j := range picked {
				maxSim = math.Max(maxSim, textutil.Jaccard(terms[i], terms[j]))
			}
			if sc := lambda*hits[i].Score - (1-lambda)*maxSim; sc > bestScore {
				best, bestScore = i, sc
			}
		}
		used[best] = true
		picked = append(picked, best)
	}
	out := make([]Hit, len(picked))
	for i, j := range picked {
		out[i] = hits[j]
	}
	return out
}

func orderSteps(steps []Step) []Step {
	order := map[string]int{"exact": 0, "filter": 1, "lexical": 2, "entity": 3, "vector": 4, "rank": 5}
	sort.SliceStable(steps, func(i, j int) bool { return order[steps[i].Strategy] < order[steps[j].Strategy] })
	return steps
}
