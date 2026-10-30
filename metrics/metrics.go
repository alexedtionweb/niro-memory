// Package metrics is a tiny, dependency-free metrics registry: latency
// histograms and error counts per operation, with percentile estimates,
// exposed as JSON (dev console) and Prometheus text format (/metrics).
// It implements niromem.Observer. For OpenTelemetry use contrib/otel.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// bounds are histogram bucket upper bounds in seconds (log-spaced 50µs–60s).
var bounds = func() []float64 {
	var b []float64
	for v := 50e-6; v < 60; v *= 1.5 {
		b = append(b, v)
	}
	return append(b, 60)
}()

// Registry holds one histogram per operation name. Safe for concurrent use.
type Registry struct {
	mu   sync.Mutex
	ops  map[string]*hist
	from time.Time
}

type hist struct {
	counts []uint64 // len(bounds)+1; last is +Inf
	n      uint64
	errs   uint64
	sum    float64
	max    float64
}

// New returns an empty registry.
func New() *Registry { return &Registry{ops: map[string]*hist{}, from: time.Now()} }

// Observe records one operation.
func (r *Registry) Observe(name string, took time.Duration, err error) {
	s := took.Seconds()
	i := sort.SearchFloat64s(bounds, s)
	r.mu.Lock()
	h := r.ops[name]
	if h == nil {
		h = &hist{counts: make([]uint64, len(bounds)+1)}
		r.ops[name] = h
	}
	h.counts[i]++
	h.n++
	h.sum += s
	h.max = math.Max(h.max, s)
	if err != nil {
		h.errs++
	}
	r.mu.Unlock()
}

// Summary is the JSON view of one operation.
type Summary struct {
	Name   string  `json:"name"`
	Count  uint64  `json:"count"`
	Errors uint64  `json:"errors"`
	Rate   float64 `json:"per_second"`
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

// Snapshot returns summaries sorted by name. Percentiles are estimated by
// linear interpolation inside histogram buckets (±25% of the true value).
func (r *Registry) Snapshot() []Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	secs := time.Since(r.from).Seconds()
	out := make([]Summary, 0, len(r.ops))
	for name, h := range r.ops {
		out = append(out, Summary{Name: name, Count: h.n, Errors: h.errs, Rate: float64(h.n) / math.Max(secs, 1),
			MeanMS: 1000 * h.sum / math.Max(float64(h.n), 1), P50MS: 1000 * h.quantile(0.5),
			P95MS: 1000 * h.quantile(0.95), P99MS: 1000 * h.quantile(0.99), MaxMS: 1000 * h.max})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *hist) quantile(q float64) float64 {
	if h.n == 0 {
		return 0
	}
	rank := q * float64(h.n)
	var seen float64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= rank {
			lo, hi := 0.0, h.max
			if i > 0 {
				lo = bounds[i-1]
			}
			if i < len(bounds) {
				hi = math.Min(bounds[i], h.max)
			}
			return lo + (hi-lo)*(rank-seen)/float64(c)
		}
		seen += float64(c)
	}
	return h.max
}

// WritePrometheus writes all histograms in the Prometheus text format.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.ops))
	for n := range r.ops {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# TYPE niromem_op_seconds histogram\n# TYPE niromem_op_errors_total counter\n")
	for _, n := range names {
		h := r.ops[n]
		label := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(n)
		var cum uint64
		for i, c := range h.counts[:len(bounds)] {
			cum += c
			fmt.Fprintf(&b, "niromem_op_seconds_bucket{op=\"%s\",le=\"%g\"} %d\n", label, bounds[i], cum)
		}
		fmt.Fprintf(&b, "niromem_op_seconds_bucket{op=\"%s\",le=\"+Inf\"} %d\n", label, h.n)
		fmt.Fprintf(&b, "niromem_op_seconds_sum{op=\"%s\"} %g\nniromem_op_seconds_count{op=\"%s\"} %d\n", label, h.sum, label, h.n)
		fmt.Fprintf(&b, "niromem_op_errors_total{op=\"%s\"} %d\n", label, h.errs)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
