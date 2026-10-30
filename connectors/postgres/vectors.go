package postgres

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

// modelRe limits model ids that may be inlined into index DDL and queries.
var modelRe = regexp.MustCompile(`^[A-Za-z0-9._@/:+-]{1,128}$`)

// EnsureVectorIndex creates an HNSW index (cosine) for one model when
// pgvector is installed. It is idempotent and safe to call from many nodes.
// Without it, or without pgvector, Search falls back to an exact scan.
func (s *Store) EnsureVectorIndex(ctx context.Context, model string, dims int) error {
	if !s.pgvector {
		return fmt.Errorf("pgvector is not installed")
	}
	if !modelRe.MatchString(model) || dims <= 0 || dims > 16000 {
		return fmt.Errorf("%w: model %q / dims %d", memory.ErrInvalid, model, dims)
	}
	name := "emb_hnsw_" + strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(model), "_"), "_")
	if len(name) > 60 {
		name = name[:60]
	}
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON embeddings USING hnsw ((vec::vector(%d)) vector_cosine_ops) WHERE model = '%s' AND dims = %d`,
		name, dims, model, dims))
	if err == nil {
		s.indexMu.Lock()
		s.indexed[model+"/"+strconv.Itoa(dims)] = true
		s.indexMu.Unlock()
	}
	return err
}

// Upsert writes points outside a batch (vector.Index).
func (s *Store) Upsert(ctx context.Context, points []vector.Point) error {
	byTenant := map[string][]vector.Point{}
	for _, p := range points {
		byTenant[p.Tenant] = append(byTenant[p.Tenant], p)
	}
	for t, pts := range byTenant {
		if _, err := s.Write(ctx, &store.Batch{Tenant: t, Vectors: pts}); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes vectors (vector.Index).
func (s *Store) Delete(ctx context.Context, tenant string, targets []vector.Target) error {
	_, err := s.Write(ctx, &store.Batch{Tenant: tenant, VectorDeletes: targets})
	return err
}

func vecLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Search uses the model's HNSW index when one exists (with pgvector's
// iterative scan so scope filters do not starve results), and an exact
// in-process scan otherwise.
func (s *Store) Search(ctx context.Context, vq vector.Query) ([]vector.Hit, error) {
	qv := append([]float32(nil), vq.Values...)
	if err := vector.Normalize(qv); err != nil {
		return nil, fmt.Errorf("%w: %v", memory.ErrInvalid, err)
	}
	conds, args := []string{"tenant = $1", "model = $2", "dims = $3"}, []any{vq.Tenant, vq.Model, len(qv)}
	for col, v := range map[string]string{"user_id": vq.User, "agent_id": vq.Agent, "run_id": vq.Run, "target_kind": string(vq.Kind)} {
		if v != "" {
			args = append(args, v)
			conds = append(conds, col+" = $"+strconv.Itoa(len(args)))
		}
	}
	s.indexMu.Lock()
	indexed := s.indexed[vq.Model+"/"+strconv.Itoa(len(qv))]
	s.indexMu.Unlock()
	var out []vector.Hit
	err := s.read(ctx, vq.Tenant, func(c q) error {
		if indexed && modelRe.MatchString(vq.Model) {
			return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
				tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`) // pgvector >= 0.8; ignored before
				d := strconv.Itoa(len(qv))
				args := append(args, vecLiteral(qv), vq.K)
				n := len(args)
				// The literal model predicate lets the planner match the partial index.
				sql := fmt.Sprintf(`SELECT target_kind, target_id, part, 1 - ((vec::vector(%s)) <=> $%d::vector(%s)) FROM embeddings
					WHERE %s AND model = '%s' ORDER BY (vec::vector(%s)) <=> $%d::vector(%s) LIMIT $%d`,
					d, n-1, d, strings.Join(conds, " AND "), vq.Model, d, n-1, d, n)
				rows, err := tx.Query(ctx, sql, args...)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var h vector.Hit
					var kind string
					if err := rows.Scan(&kind, &h.Target.ID, &h.Target.Part, &h.Score); err != nil {
						return err
					}
					h.Target.Kind = vector.TargetKind(kind)
					out = append(out, h)
				}
				return rows.Err()
			})
		}
		rows, err := c.Query(ctx, `SELECT target_kind, target_id, part, vec FROM embeddings WHERE `+strings.Join(conds, " AND "), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		top := vector.NewTopK(vq.K)
		for rows.Next() {
			var h vector.Hit
			var kind string
			var v []float32
			if err := rows.Scan(&kind, &h.Target.ID, &h.Target.Part, &v); err != nil {
				return err
			}
			h.Target.Kind = vector.TargetKind(kind)
			h.Score = float64(vector.Dot(qv, v))
			top.Push(h)
		}
		out = top.Sorted()
		return rows.Err()
	})
	return out, err
}

// Vectors pages through stored points of a model.
func (s *Store) Vectors(ctx context.Context, tenant, model, after string, limit int) ([]vector.Point, error) {
	if limit <= 0 {
		limit = 1000
	}
	var out []vector.Point
	err := s.read(ctx, tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT target_kind, target_id, part, modality, user_id, agent_id, run_id, vec FROM embeddings
			WHERE tenant = $1 AND model = $2 AND target_key > $3 ORDER BY target_key LIMIT $4`, tenant, model, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p := vector.Point{Tenant: tenant, Model: model}
			var kind string
			if err := rows.Scan(&kind, &p.Target.ID, &p.Target.Part, &p.Modality, &p.User, &p.Agent, &p.Run, &p.Values); err != nil {
				return err
			}
			p.Target.Kind = vector.TargetKind(kind)
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// Missing lists open memories without a vector for model, oldest first.
func (s *Store) Missing(ctx context.Context, tenant, model string, limit int) ([]vector.Target, error) {
	if limit <= 0 {
		limit = 1000
	}
	var out []vector.Target
	err := s.read(ctx, tenant, func(c q) error {
		rows, err := c.Query(ctx, `SELECT m.id FROM memories m WHERE m.tenant = $1 AND m.valid_to IS NULL AND m.invalidated_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM embeddings v WHERE v.tenant = m.tenant AND v.model = $2 AND v.target_key = 'm:' || m.id)
			ORDER BY m.id LIMIT $3`, tenant, model, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := vector.Target{Kind: vector.TargetMemory}
			if err := rows.Scan(&t.ID); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// Coverage counts open memories with and without a vector for model.
func (s *Store) Coverage(ctx context.Context, tenant, model string) (have, total int, err error) {
	err = s.read(ctx, tenant, func(c q) error {
		return c.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM embeddings v WHERE v.tenant = m.tenant AND v.model = $2 AND v.target_key = 'm:' || m.id))
			FROM memories m WHERE m.tenant = $1 AND m.valid_to IS NULL AND m.invalidated_at IS NULL`, tenant, model).Scan(&total, &have)
	})
	return have, total, err
}
