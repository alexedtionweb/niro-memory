package vector

import (
	"container/heap"
	"context"
	"strings"
	"sync"
)

// TopK keeps the k best hits seen so far. It is the building block of every
// brute-force scan: the in-process Flat index and SQL connectors that scan
// stored vectors. Use NewTopK to create one.
type TopK struct {
	k int
	h hitHeap
}

// NewTopK returns a collector for the k highest-scoring hits.
func NewTopK(k int) *TopK { return &TopK{k: k, h: make(hitHeap, 0, k)} }

// Push offers a hit; it is kept only if it is among the k best.
func (t *TopK) Push(h Hit) {
	if t.k <= 0 {
		return
	}
	if len(t.h) < t.k {
		heap.Push(&t.h, h)
		return
	}
	if h.Score > t.h[0].Score {
		t.h[0] = h
		heap.Fix(&t.h, 0)
	}
}

// Sorted returns the kept hits from best to worst.
func (t *TopK) Sorted() []Hit {
	h := append(hitHeap(nil), t.h...)
	out := make([]Hit, len(h))
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(&h).(Hit)
	}
	return out
}

type hitHeap []Hit // min-heap on Score

func (h hitHeap) Len() int           { return len(h) }
func (h hitHeap) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h hitHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)        { *h = append(*h, x.(Hit)) }
func (h *hitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Flat is an exact, in-process brute-force Index. It suits tests and small
// embedded deployments. It holds vectors in process memory, so stateless
// deployments should use the store's own index or an external connector.
type Flat struct {
	mu     sync.RWMutex
	points map[string]map[string]Point // tenant\x00model -> target key -> point
}

// NewFlat returns an empty Flat index.
func NewFlat() *Flat { return &Flat{points: map[string]map[string]Point{}} }

func space(tenant, model string) string { return tenant + "\x00" + model }

// Upsert stores normalized copies of the points.
func (f *Flat) Upsert(_ context.Context, points []Point) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range points {
		p.Values = append([]float32(nil), p.Values...)
		if err := Normalize(p.Values); err != nil {
			return err
		}
		s := space(p.Tenant, p.Model)
		if f.points[s] == nil {
			f.points[s] = map[string]Point{}
		}
		f.points[s][p.Target.Key()] = p
	}
	return nil
}

// Delete removes the targets from every model of the tenant.
func (f *Flat) Delete(_ context.Context, tenant string, targets []Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s, pts := range f.points {
		if !strings.HasPrefix(s, tenant+"\x00") {
			continue
		}
		for _, t := range targets {
			delete(pts, t.Key())
		}
	}
	return nil
}

// Search scans every point of the tenant and model.
func (f *Flat) Search(_ context.Context, q Query) ([]Hit, error) {
	qv := append([]float32(nil), q.Values...)
	if err := Normalize(qv); err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	top := NewTopK(q.K)
	for _, p := range f.points[space(q.Tenant, q.Model)] {
		if !Matches(p, q) {
			continue
		}
		if len(p.Values) != len(qv) {
			return nil, ErrDims
		}
		top.Push(Hit{Target: p.Target, Score: float64(Dot(qv, p.Values))})
	}
	return top.Sorted(), nil
}

// Matches reports whether a point passes the query's scope prefilter.
func Matches(p Point, q Query) bool {
	return (q.User == "" || p.User == q.User) &&
		(q.Agent == "" || p.Agent == q.Agent) &&
		(q.Run == "" || p.Run == q.Run) &&
		(q.Kind == "" || p.Target.Kind == q.Kind)
}
