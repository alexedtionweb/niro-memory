// Package niromem is a memory engine for AI agents: fast LLM-free
// retrieval, versioned bi-temporal knowledge, conversation history with
// multimodal parts, and durable background processing — over any storage
// backend.
//
//	db, _ := sqlite.Open(ctx, sqlite.Options{Path: "memory.db"})
//	mem, _ := niromem.Open(niromem.Options{Store: db})
//	mem.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "acme", User: "alice"},
//		Text: "Alice lives in Lisbon", Slot: "user.location"})
//	res, _ := mem.Search(ctx, niromem.SearchRequest{Tenant: "acme", User: "alice", Query: "where does alice live?"})
//
// Everything is wired explicitly through Options: the store is required;
// a vector index, blob store, cache, embedder and rules are optional.
package niromem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/cache"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/pipeline"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Options wires an Engine. Only Store is required.
type Options struct {
	// Store is the source of truth (connectors/sqlite, connectors/postgres, …).
	Store store.Store
	// Vector is an external vector index (e.g. Qdrant). When nil, the
	// store's own index is used if it implements vector.Index.
	Vector vector.Index
	// Blob stores binary episode parts. Without it, parts must be URLs or
	// references the caller already stores.
	Blob blob.Store
	// Cache speeds up settings and query embeddings (default: in-process LRU).
	Cache cache.Cache
	// Embedder computes missing vectors in the background and query vectors
	// within the policy budget.
	Embedder embed.Embedder
	// Rules extract memories from conversation turns. A tenant may override
	// them in its settings.
	Rules *rules.Set
	// Extractors are extension points (e.g. LLM extraction) run after rules.
	Extractors []pipeline.Extractor
	Limits     memory.Limits
	Logger     *slog.Logger
	Now        func() time.Time
	// Debounce delays processing of appended turns so bursts are processed
	// together (default 1s). Append with Wait processes immediately.
	Debounce time.Duration
	// SettingsTTL bounds how stale cached tenant settings may be (default 5s).
	SettingsTTL time.Duration
	// Observer receives metrics (optional).
	Observer Observer
}

// Observer receives engine measurements. metrics.Registry implements it.
type Observer interface {
	Observe(name string, took time.Duration, err error)
}

// Engine is the memory engine. It is safe for concurrent use and keeps no
// state that correctness depends on: any number of engines may share a store.
type Engine struct {
	o        Options
	index    vector.Index
	external bool
	rec      *pipeline.Reconciler
	proc     *pipeline.Processor
	search   *search.Searcher

	rulesMu sync.Mutex
	rulesBy map[string]*rules.Set // parsed tenant rule documents by content

	heatMu   sync.Mutex
	heat     map[string]map[string]int64 // tenant -> memory -> pending increments
	heatLast time.Time
	heatWG   sync.WaitGroup
}

// Open validates options and returns an Engine. It performs no I/O.
func Open(o Options) (*Engine, error) {
	if o.Store == nil {
		return nil, errors.New("niromem: Options.Store is required")
	}
	if o.Limits == (memory.Limits{}) {
		o.Limits = memory.DefaultLimits
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Cache == nil {
		o.Cache = cache.NewLRU(10000)
	}
	if o.Debounce == 0 {
		o.Debounce = time.Second
	}
	if o.SettingsTTL == 0 {
		o.SettingsTTL = 5 * time.Second
	}
	e := &Engine{o: o, index: o.Vector, external: o.Vector != nil, rulesBy: map[string]*rules.Set{},
		heat: map[string]map[string]int64{}, heatLast: o.Now()}
	if e.index == nil {
		if idx, ok := o.Store.(vector.Index); ok {
			e.index = idx
		}
	}
	e.rec = &pipeline.Reconciler{Store: o.Store, Now: o.Now, Limits: o.Limits}
	e.proc = &pipeline.Processor{Store: o.Store, Reconciler: e.rec, RulesFor: e.rulesFor,
		Extractors: o.Extractors, OnWrite: e.followUp}
	e.search = &search.Searcher{Store: o.Store, Index: e.index, Embedder: o.Embedder, Cache: o.Cache,
		RulesFor: e.rulesFor, Now: o.Now}
	return e, nil
}

// Close flushes pending heat counters. It does not close the store, which
// the caller owns.
func (e *Engine) Close() error {
	e.flushHeat(true)
	e.heatWG.Wait()
	return nil
}

// Store returns the underlying store (for tools such as the dev console).
func (e *Engine) Store() store.Store { return e.o.Store }

// Capabilities describes what this engine can do with its configuration.
type Capabilities struct {
	Lexical  bool   `json:"lexical"`
	BM25     string `json:"bm25,omitempty"`
	Entities bool   `json:"entities"`
	Vector   bool   `json:"vector"`
	External bool   `json:"external_vector_index"`
	Embedder string `json:"embedder,omitempty"`
	Blob     bool   `json:"blob"`
	Rules    string `json:"rules,omitempty"`
}

// Capabilities reports the features available with the configured connectors.
func (e *Engine) Capabilities() Capabilities {
	c := Capabilities{Vector: e.index != nil, External: e.external, Blob: e.o.Blob != nil}
	_, c.Lexical = e.o.Store.(store.Lexical)
	_, c.Entities = e.o.Store.(store.Entities)
	if d, ok := e.o.Store.(store.Describer); ok {
		c.BM25 = d.Capabilities().BM25
	}
	if e.o.Embedder != nil {
		c.Embedder = e.o.Embedder.Model().ID
	}
	if e.o.Rules != nil {
		c.Rules = e.o.Rules.Version
	}
	return c
}

func (e *Engine) observe(name string, start time.Time, err error) {
	if e.o.Observer != nil {
		e.o.Observer.Observe(name, time.Since(start), err)
	}
}

// Job kinds handled by the engine's worker.
const (
	JobProcess  = "process"  // extract memories from new turns of a run
	JobRederive = "rederive" // re-extract an edited or deleted turn
	JobEmbed    = "embed"    // fill missing vectors with the embedder
	JobIndex    = "index"    // copy vectors to the external index
	JobUnindex  = "unindex"  // remove vectors from the external index
)

// Worker returns a job runner with the engine's handlers. Run it in-process
// (go w.Run(ctx)), in a separate `niromem worker` process, or call RunOnce
// from a serverless function.
func (e *Engine) Worker() *jobs.Runner {
	return &jobs.Runner{Queue: e.o.Store, Logger: e.o.Logger, Now: e.o.Now,
		Observe: func(kind string, took time.Duration, err error) {
			if e.o.Observer != nil {
				e.o.Observer.Observe("job."+kind, took, err)
			}
		},
		Handlers: map[string]jobs.Handler{
			JobProcess:  e.handleProcess,
			JobRederive: e.handleRederive,
			JobEmbed:    e.handleEmbed,
			JobIndex:    e.handleIndex,
			JobUnindex:  e.handleUnindex,
		}}
}

type runPayload struct {
	Run     string `json:"run,omitempty"`
	Episode string `json:"episode,omitempty"`
}

func payload(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (e *Engine) handleProcess(ctx context.Context, j store.Job) error {
	var p runPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil || p.Run == "" {
		return jobs.Permanent(fmt.Errorf("bad payload: %s", j.Payload))
	}
	_, err := e.proc.ProcessRun(ctx, j.Tenant, p.Run)
	return err
}

func (e *Engine) handleRederive(ctx context.Context, j store.Job) error {
	var p runPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil || p.Episode == "" {
		return jobs.Permanent(fmt.Errorf("bad payload: %s", j.Payload))
	}
	_, err := e.proc.Rederive(ctx, j.Tenant, p.Episode)
	return err
}

// followUp adds background work for newly added memories to a batch. The
// embed job is keyed per model and debounced like turn processing, so
// bursts of writes are embedded in batches.
func (e *Engine) followUp(b *store.Batch, changes []pipeline.Change) {
	if e.o.Embedder == nil {
		return
	}
	for _, c := range changes {
		if c.Action == memory.ActionAdd || c.Action == memory.ActionSupersede {
			b.Jobs = append(b.Jobs, store.Job{Kind: JobEmbed, Key: e.o.Embedder.Model().ID, NotBefore: e.o.Now().Add(e.o.Debounce)})
			return
		}
	}
}

// write applies a batch, re-planning with plan on conflicts.
func (e *Engine) write(ctx context.Context, plan func() (*store.Batch, error)) (*store.Receipt, error) {
	for attempt := 0; ; attempt++ {
		b, err := plan()
		if err != nil {
			return nil, err
		}
		rec, err := e.o.Store.Write(ctx, b)
		if errors.Is(err, memory.ErrConflict) && attempt < 5 {
			continue
		}
		return rec, err
	}
}

// bumpHeat records retrieval hits. Heat is a ranking hint, not data: it is
// buffered in memory and flushed in the background (losing a few
// increments on a crash is acceptable).
func (e *Engine) bumpHeat(tenant string, ids []string) {
	if len(ids) == 0 {
		return
	}
	e.heatMu.Lock()
	m := e.heat[tenant]
	if m == nil {
		m = map[string]int64{}
		e.heat[tenant] = m
	}
	for _, id := range ids {
		m[id]++
	}
	e.heatMu.Unlock()
	e.flushHeat(false)
}

func (e *Engine) flushHeat(force bool) {
	e.heatMu.Lock()
	n := 0
	for _, m := range e.heat {
		n += len(m)
	}
	if n == 0 || (!force && n < 64 && e.o.Now().Sub(e.heatLast) < 5*time.Second) {
		e.heatMu.Unlock()
		return
	}
	pending := e.heat
	e.heat, e.heatLast = map[string]map[string]int64{}, e.o.Now()
	e.heatMu.Unlock()
	e.heatWG.Add(1)
	go func() {
		defer e.heatWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for tenant, m := range pending {
			b := &store.Batch{Tenant: tenant}
			for id, n := range m {
				b.Touches = append(b.Touches, store.Touch{ID: id, Heat: n})
			}
			if _, err := e.o.Store.Write(ctx, b); err != nil {
				e.o.Logger.Warn("heat flush failed", "tenant", tenant, "err", err)
			}
		}
	}()
}
