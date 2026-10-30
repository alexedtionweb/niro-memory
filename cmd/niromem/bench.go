package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

// bench measures the engine on a synthetic dataset. Every number it prints
// is measured in this run; the report includes the environment so results
// can be reproduced and compared.
func bench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	path := fs.String("config", "", "config file (default: SQLite in a temp dir)")
	n := fs.Int("n", 100_000, "memories to load")
	tenants := fs.Int("tenants", 10, "tenants")
	users := fs.Int("users", 100, "users per tenant")
	dims := fs.Int("dims", 64, "vector dimensions (hash embedder); 0 disables vectors")
	ops := fs.Int("ops", 2000, "operations per measurement")
	conc := fs.String("concurrency", "1,8,32", "concurrency levels")
	out := fs.String("out", "", "write the JSON report here")
	seed := fs.Uint64("seed", 1, "random seed")
	only := fs.String("only", "", "run only scenarios whose name contains this")
	cpuprofile := fs.String("cpuprofile", "", "write a CPU profile of the measurements")
	fs.Parse(args)

	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	tmp := ""
	if *path == "" {
		tmp, _ = os.MkdirTemp("", "niromem-bench-*")
		defer os.RemoveAll(tmp)
		cfg.Store.Options = map[string]json.RawMessage{"sqlite": json.RawMessage(fmt.Sprintf(`{"path":%q}`, filepath.Join(tmp, "bench.db")))}
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	e, closeAll, err := open(ctx, cfg, quiet, nil)
	if err != nil {
		return err
	}
	defer closeAll()
	ds := newDataset(*seed, *tenants, *users, *dims)
	rep := &report{Env: environment(cfg), Params: map[string]any{"n": *n, "tenants": *tenants, "users_per_tenant": *users, "dims": *dims, "ops": *ops, "seed": *seed}}
	fmt.Fprintf(os.Stderr, "loading %d memories (%d tenants × %d users)…\n", *n, *tenants, *users)
	loadStart := time.Now()
	ids, err := ds.load(ctx, e.Store(), *n)
	if err != nil {
		return err
	}
	rep.Load = map[string]any{"seconds": time.Since(loadStart).Seconds(), "per_second": float64(*n) / time.Since(loadStart).Seconds(), "path": "bulk store.Write, 1000 per batch"}
	if tmp != "" {
		rep.Load["db_bytes"] = dirSize(tmp)
	}
	levels := parseInts(*conc)
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	for _, s := range scenarios(e, ds, ids, *dims > 0) {
		if *only != "" && !strings.Contains(s.name, *only) {
			continue
		}
		for i, c := range levels {
			r := measure(ctx, s, c, *ops, i == 0)
			rep.Results = append(rep.Results, r)
			fmt.Fprintf(os.Stderr, "%-26s c=%-3d p50=%7.3fms p95=%7.3fms p99=%7.3fms %8.0f ops/s errors=%d\n", r.Name, c, r.P50, r.P95, r.P99, r.QPS, r.Errors)
		}
	}
	if *only == "" {
		rep.Ingest = ingestLatency(ctx, e, *ops/4)
		rep.Interference = interference(ctx, e, ds, *ops)
	}
	rep.Resources = resources()
	b, _ := json.MarshalIndent(rep, "", "  ")
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(rep.markdown())
	return nil
}

type report struct {
	Env          map[string]any `json:"environment"`
	Params       map[string]any `json:"params"`
	Load         map[string]any `json:"load"`
	Results      []result       `json:"results"`
	Ingest       []result       `json:"ingest"`
	Interference []result       `json:"interference"`
	Resources    map[string]any `json:"resources"`
}

type result struct {
	Name        string  `json:"name"`
	Concurrency int     `json:"concurrency"`
	Ops         int     `json:"ops"`
	Errors      int64   `json:"errors"`
	P50         float64 `json:"p50_ms"`
	P95         float64 `json:"p95_ms"`
	P99         float64 `json:"p99_ms"`
	Max         float64 `json:"max_ms"`
	QPS         float64 `json:"ops_per_second"`
	Cold        float64 `json:"cold_first_op_ms,omitempty"`
}

func environment(cfg *Config) map[string]any {
	host, _ := os.Hostname()
	cpu := "unknown"
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				cpu = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
	}
	return map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(),
		"cpu": cpu, "host": host, "store": cfg.Store.Use, "vector": cfg.Vector.Use, "date": time.Now().UTC().Format(time.RFC3339)}
}

// dataset generates memories from a Zipf-distributed synthetic vocabulary,
// so term frequencies resemble natural text.
type dataset struct {
	mu      sync.Mutex
	r       *rand.Rand
	zipf    *rand.Zipf
	words   []string
	tenants int
	users   int
	emb     embed.Embedder
}

func newDataset(seed uint64, tenants, users, dims int) *dataset {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	syl := []string{"ka", "lo", "mi", "ra", "te", "su", "no", "vi", "pe", "da", "ri", "zo", "an", "el", "or", "us"}
	words := make([]string, 20000)
	for i := range words {
		var b strings.Builder
		for j := 0; j < 2+i%3; j++ {
			b.WriteString(syl[r.IntN(len(syl))])
		}
		words[i] = fmt.Sprintf("%s%d", b.String(), i%97)
	}
	d := &dataset{r: r, zipf: rand.NewZipf(r, 1.1, 2, uint64(len(words)-1)), words: words, tenants: tenants, users: users}
	if dims > 0 {
		d.emb = embed.Hash{Dims: dims}
	}
	return d
}

func (d *dataset) text(n int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := make([]string, n)
	for i := range w {
		w[i] = d.words[d.zipf.Uint64()]
	}
	return strings.Join(w, " ")
}

func (d *dataset) pick(n int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.r.IntN(n)
}

func (d *dataset) scope(i int) memory.Scope {
	t := i % d.tenants
	u := (i / d.tenants) % d.users
	return memory.Scope{Tenant: fmt.Sprintf("t%d", t), User: fmt.Sprintf("u%d", u), Agent: "a"}
}

// load writes memories (and their vectors) with bulk batches.
func (d *dataset) load(ctx context.Context, s store.Store, n int) ([]memory.Memory, error) {
	now := time.Now().UTC()
	sample := make([]memory.Memory, 0, 4096)
	byTenant := map[string]*store.Batch{}
	flush := func(b *store.Batch) error {
		if b.Empty() {
			return nil
		}
		_, err := s.Write(ctx, b)
		b.Memories, b.Vectors = b.Memories[:0], b.Vectors[:0]
		return err
	}
	for i := 0; i < n; i++ {
		sc := d.scope(i)
		m := memory.Memory{ID: memory.NewID(), Scope: sc, Kind: memory.Semantic, Layer: memory.Atom, Text: d.text(8 + i%12),
			Metadata: map[string]any{"category": fmt.Sprintf("c%d", i%50)}, Confidence: 0.8, Importance: 0.5,
			Visibility: memory.Team, ValidFrom: now.Add(-time.Duration(i%10000) * time.Minute), RecordedAt: now, Version: 1,
			Source: memory.Source{Kind: "import", Trust: memory.TrustNormal}}
		if i%10 == 0 {
			m.Entities = []memory.EntityRef{{Type: "topic", Name: d.words[d.pick(500)]}}
		}
		m.Hash = memory.Hash(m.Text)
		b := byTenant[sc.Tenant]
		if b == nil {
			b = &store.Batch{Tenant: sc.Tenant}
			byTenant[sc.Tenant] = b
		}
		b.Memories = append(b.Memories, m)
		if d.emb != nil {
			v, _, _ := d.emb.Embed(ctx, embed.Document, []embed.Input{{Text: m.Text}})
			b.Vectors = append(b.Vectors, vector.Point{Model: d.emb.Model().ID, Target: vector.Target{Kind: vector.TargetMemory, ID: m.ID}, User: sc.User, Agent: sc.Agent, Values: v[0]})
		}
		if len(sample) < cap(sample) && i%max(n/cap(sample), 1) == 0 {
			sample = append(sample, m)
		}
		if len(b.Memories) == 1000 {
			if err := flush(b); err != nil {
				return nil, err
			}
		}
		if i > 0 && i%100_000 == 0 {
			fmt.Fprintf(os.Stderr, "  %d\n", i)
		}
	}
	for _, b := range byTenant {
		if err := flush(b); err != nil {
			return nil, err
		}
	}
	return sample, nil
}

type scenario struct {
	name string
	op   func(ctx context.Context, i int) error
}

func scenarios(e *niromem.Engine, d *dataset, sample []memory.Memory, vectors bool) []scenario {
	query := func(i int) (memory.Memory, string) {
		m := sample[i%len(sample)]
		words := strings.Fields(m.Text)
		j := d.pick(max(len(words)-2, 1))
		return m, strings.Join(words[j:min(j+3, len(words))], " ")
	}
	ss := []scenario{
		{"exact (Get by id)", func(ctx context.Context, i int) error {
			m := sample[i%len(sample)]
			_, err := e.Get(ctx, m.Tenant, memory.Principal{Admin: true}, m.ID)
			return err
		}},
		{"metadata (user + filter)", func(ctx context.Context, i int) error {
			m := sample[i%len(sample)]
			_, err := e.List(ctx, niromem.ListRequest{Tenant: m.Tenant, User: m.User, Limit: 20,
				Filter: memory.Eq("metadata.category", m.Metadata["category"])})
			return err
		}},
		{"lexical (policy fast)", func(ctx context.Context, i int) error {
			m, q := query(i)
			_, err := e.Search(ctx, niromem.SearchRequest{Tenant: m.Tenant, User: m.User, Query: q, Limit: 10, Policy: "fast"})
			return err
		}},
	}
	if vectors {
		ss = append(ss, scenario{"hybrid (lexical+entity+vector)", func(ctx context.Context, i int) error {
			m, q := query(i)
			v, _, _ := d.emb.Embed(ctx, embed.Query, []embed.Input{{Text: q}})
			_, err := e.Search(ctx, niromem.SearchRequest{Tenant: m.Tenant, User: m.User, Query: q, Limit: 10,
				Vector: &search.QueryVector{Model: d.emb.Model().ID, Values: v[0]}})
			return err
		}})
	}
	ss = append(ss, scenario{"context (layered, budgeted)", func(ctx context.Context, i int) error {
		m, q := query(i)
		_, err := e.Context(ctx, niromem.ContextRequest{Tenant: m.Tenant, User: m.User, Query: q, Policy: "fast"})
		return err
	}})
	return ss
}

func measure(ctx context.Context, s scenario, conc, ops int, cold bool) result {
	r := result{Name: s.name, Concurrency: conc, Ops: ops}
	if cold {
		t := time.Now()
		s.op(ctx, 0)
		r.Cold = ms(time.Since(t))
	} else {
		for i := 0; i < 50; i++ { // warm up
			s.op(ctx, i)
		}
	}
	lat := make([]time.Duration, ops)
	var next atomic.Int64
	var errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= ops {
					return
				}
				t := time.Now()
				if err := s.op(ctx, i+1); err != nil {
					errs.Add(1)
				}
				lat[i] = time.Since(t)
			}
		}()
	}
	wg.Wait()
	total := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	q := func(p float64) float64 { return ms(lat[min(int(p*float64(len(lat))), len(lat)-1)]) }
	r.P50, r.P95, r.P99, r.Max, r.QPS, r.Errors = q(0.5), q(0.95), q(0.99), ms(lat[len(lat)-1]), float64(ops)/total.Seconds(), errs.Load()
	return r
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ingestLatency measures the synchronous write paths.
func ingestLatency(ctx context.Context, e *niromem.Engine, ops int) []result {
	n := atomic.Int64{}
	return []result{
		measure(ctx, scenario{"remember (explicit fact)", func(ctx context.Context, i int) error {
			_, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "ingest", User: fmt.Sprintf("u%d", i%50)},
				Text: fmt.Sprintf("ingest fact %d about topic %d", n.Add(1), i%300), Slot: fmt.Sprintf("s.%d", i%20)})
			return err
		}}, 8, ops, false),
		measure(ctx, scenario{"append (turn, async processing)", func(ctx context.Context, i int) error {
			_, err := e.Append(ctx, memory.Scope{Tenant: "ingest", User: "u", Run: fmt.Sprintf("r%d", i%64)},
				[]niromem.Turn{{Role: "user", Text: fmt.Sprintf("turn %d: I live in City%d", n.Add(1), i%9)}}, niromem.AppendOptions{})
			return err
		}}, 8, ops, false),
	}
}

// interference measures lexical retrieval alone, then while turns are
// appended and processed by workers at full speed in the same process.
func interference(ctx context.Context, e *niromem.Engine, d *dataset, ops int) []result {
	sample := func(i int) (string, string, string) {
		sc := d.scope(i)
		return sc.Tenant, sc.User, d.text(3)
	}
	search := scenario{"search while idle", func(ctx context.Context, i int) error {
		t, u, q := sample(i)
		_, err := e.Search(ctx, niromem.SearchRequest{Tenant: t, User: u, Query: q, Policy: "fast"})
		return err
	}}
	idle := measure(ctx, search, 8, ops, false)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var appended atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				e.Append(ctx, memory.Scope{Tenant: "load", User: "u", Run: fmt.Sprintf("w%d-%d", w, i%16)},
					[]niromem.Turn{{Role: "user", Text: fmt.Sprintf("My name is Person%d. I live in Town%d.", i, i%7)}}, niromem.AppendOptions{})
				appended.Add(1)
			}
		}(w)
	}
	worker := e.Worker()
	worker.Concurrency = 4
	wctx, cancel := context.WithCancel(ctx)
	wg.Add(1)
	go func() { defer wg.Done(); worker.Run(wctx) }()
	search.name = "search while ingesting"
	busy := measure(ctx, search, 8, ops, false)
	close(stop)
	cancel()
	wg.Wait()
	busy.Name = fmt.Sprintf("search while ingesting (%d turns appended)", appended.Load())
	return []result{idle, busy}
}

func resources() map[string]any {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := map[string]any{
		"cpu_user_s":   float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6,
		"cpu_system_s": float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6,
		"max_rss_mb":   float64(ru.Maxrss) / 1024,
		"go_heap_mb":   float64(ms.HeapAlloc) / (1 << 20),
		"goroutines":   runtime.NumGoroutine(),
	}
	return out
}

func dirSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func parseInts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		var v int
		if _, err := fmt.Sscan(strings.TrimSpace(f), &v); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func (r *report) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Environment: %v\n\nParameters: %v\n\nLoad: %v\n\n", r.Env, r.Params, r.Load)
	table := func(title string, rs []result) {
		fmt.Fprintf(&b, "%s\n\n| scenario | conc | p50 ms | p95 ms | p99 ms | max ms | ops/s | errors | cold first op ms |\n|---|---|---|---|---|---|---|---|---|\n", title)
		for _, x := range rs {
			cold := ""
			if x.Cold > 0 {
				cold = fmt.Sprintf("%.3f", x.Cold)
			}
			fmt.Fprintf(&b, "| %s | %d | %.3f | %.3f | %.3f | %.3f | %.0f | %d | %s |\n", x.Name, x.Concurrency, x.P50, x.P95, x.P99, x.Max, x.QPS, x.Errors, cold)
		}
		b.WriteString("\n")
	}
	table("Retrieval", r.Results)
	table("Ingestion (synchronous part)", r.Ingest)
	table("Interference", r.Interference)
	fmt.Fprintf(&b, "Resources: %v\n", r.Resources)
	return b.String()
}
