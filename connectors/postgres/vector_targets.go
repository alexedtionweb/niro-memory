package postgres

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
	var out []vector.Point
	err := s.read(ctx, tenant, func(c q) error {
		out = nil
		rows, err := c.Query(ctx, `SELECT model, target_kind, target_id, part, modality, user_id, agent_id, run_id, vec FROM embeddings
			WHERE tenant = $1 AND target_id = ANY($2::text[]) AND target_key = ANY($3::text[])
			ORDER BY target_key COLLATE "C", model COLLATE "C" LIMIT $4`, tenant, ids, keys, store.MaxTargetVectors+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(out) == store.MaxTargetVectors {
				return store.ErrTooManyTargetVectors
			}
			p := vector.Point{Tenant: tenant}
			var kind string
			if err := rows.Scan(&p.Model, &kind, &p.Target.ID, &p.Target.Part, &p.Modality, &p.User, &p.Agent, &p.Run, &p.Values); err != nil {
				return err
			}
			p.Target.Kind = vector.TargetKind(kind)
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
