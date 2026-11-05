package memstore

import (
	"context"
	"sort"

	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

var _ store.VectorTargets = (*Store)(nil)

// TargetVectors returns stored vectors of explicit targets across all models,
// ordered by target key, then model. Values are copies.
func (s *Store) TargetVectors(_ context.Context, tenant string, targets []vector.Target) ([]vector.Point, error) {
	if err := store.ValidateVectorTargets(tenant, targets); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(targets))
	seen := map[string]bool{}
	for _, t := range targets {
		if k := t.Key(); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tenants[tenant]
	if t == nil {
		return nil, nil
	}
	models := make([]string, 0, len(t.vectors))
	for m := range t.vectors {
		models = append(models, m)
	}
	sort.Strings(models)
	var out []vector.Point
	for _, k := range keys {
		for _, m := range models {
			p, ok := t.vectors[m][k]
			if !ok {
				continue
			}
			if len(out) == store.MaxTargetVectors {
				return nil, store.ErrTooManyTargetVectors
			}
			p.Values = append([]float32(nil), p.Values...)
			out = append(out, p)
		}
	}
	return out, nil
}
