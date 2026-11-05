package sqlite

import (
	"context"
	"sort"

	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

var _ store.VectorTargets = (*Store)(nil)

// TargetVectors returns stored vectors of explicit targets across all models,
// ordered by target key, then model. It uses the (tenant, target_id) index
// and reads at most MaxTargetVectors+1 rows.
func (s *Store) TargetVectors(ctx context.Context, tenant string, targets []vector.Target) ([]vector.Point, error) {
	if err := store.ValidateVectorTargets(tenant, targets); err != nil {
		return nil, err
	}
	ids, keys := targetArgs(targets)
	if len(keys) == 0 {
		return nil, nil
	}
	args := make([]any, 0, 2+len(ids)+len(keys))
	args = append(args, tenant)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, store.MaxTargetVectors+1)
	rows, err := s.r.QueryContext(ctx, `SELECT model, target_kind, target_id, part, modality, user_id, agent_id, run_id, vec FROM embeddings
		WHERE tenant = ? AND target_id IN (`+placeholders(len(ids))+`) AND target_key IN (`+placeholders(len(keys))+`)
		ORDER BY target_key, model LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []vector.Point
	for rows.Next() {
		if len(out) == store.MaxTargetVectors {
			return nil, store.ErrTooManyTargetVectors
		}
		p := vector.Point{Tenant: tenant}
		var raw []byte
		if err := rows.Scan(&p.Model, &p.Target.Kind, &p.Target.ID, &p.Target.Part, &p.Modality, &p.User, &p.Agent, &p.Run, &raw); err != nil {
			return nil, err
		}
		p.Values = decodeVec(raw, nil)
		out = append(out, p)
	}
	return out, rows.Err()
}

func targetArgs(targets []vector.Target) (ids, keys []string) {
	seenID, seenKey := map[string]bool{}, map[string]bool{}
	for _, t := range targets {
		if !seenID[t.ID] {
			seenID[t.ID] = true
			ids = append(ids, t.ID)
		}
		if k := t.Key(); !seenKey[k] {
			seenKey[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(ids)
	sort.Strings(keys)
	return ids, keys
}
