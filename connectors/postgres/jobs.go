package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alexedtionweb/niro-memory/store"
)

const jobCols = `id, tenant, kind, key, payload, state, attempts, not_before, lease_until, worker, checkpoint, last_error, created_at, updated_at`

func scanJob(r pgx.Row) (store.Job, error) {
	var j store.Job
	var state string
	var lease *time.Time
	var payload, cp []byte
	err := r.Scan(&j.ID, &j.Tenant, &j.Kind, &j.Key, &payload, &state, &j.Attempts, &j.NotBefore, &lease, &j.Worker, &cp, &j.LastError, &j.CreatedAt, &j.UpdatedAt)
	j.State, j.NotBefore, j.CreatedAt, j.UpdatedAt = store.JobState(state), j.NotBefore.UTC(), j.CreatedAt.UTC(), j.UpdatedAt.UTC()
	if lease != nil {
		j.LeaseUntil = lease.UTC()
	}
	if len(payload) > 0 {
		j.Payload = json.RawMessage(payload)
	}
	if len(cp) > 0 {
		j.Checkpoint = json.RawMessage(cp)
	}
	return j, err
}

// Claim leases due jobs. Candidates are ranked round-robin per tenant,
// locked with FOR UPDATE SKIP LOCKED (concurrent workers never wait on each
// other), and a per-key transaction advisory lock stops two workers from
// claiming jobs of the same key at the same moment.
func (s *Store) Claim(ctx context.Context, worker string, n int, lease time.Duration) ([]store.Job, error) {
	var out []store.Job
	now := utc(s.now())
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH ranked AS (
			SELECT j.id, row_number() OVER (PARTITION BY j.tenant ORDER BY j.not_before, j.id) AS rn
			FROM jobs j
			WHERE ((j.state = 'pending' AND j.not_before <= $1) OR (j.state = 'leased' AND j.lease_until <= $1))
			AND (j.key = '' OR NOT EXISTS (SELECT 1 FROM jobs b WHERE b.tenant = j.tenant AND b.kind = j.kind AND b.key = j.key
				AND b.state = 'leased' AND b.lease_until > $1))
			ORDER BY rn LIMIT $2)
		SELECT j.id, j.tenant, j.kind, j.key FROM jobs j JOIN ranked r ON r.id = j.id
		WHERE j.key = '' OR pg_try_advisory_xact_lock(hashtextextended(j.tenant || '/' || j.kind || '/' || j.key, 0))
		ORDER BY r.rn
		FOR UPDATE OF j SKIP LOCKED`, now, n*4)
		if err != nil {
			return err
		}
		var ids []string
		taken := map[string]bool{}
		for rows.Next() {
			var id, tenant, kind, key string
			if err := rows.Scan(&id, &tenant, &kind, &key); err != nil {
				rows.Close()
				return err
			}
			k := tenant + "\x00" + kind + "\x00" + key
			if len(ids) == n || (key != "" && taken[k]) {
				continue
			}
			taken[k] = true
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			j, err := scanJob(tx.QueryRow(ctx, `UPDATE jobs SET state = 'leased', worker = $1, lease_until = $2, attempts = attempts + 1, updated_at = $3
				WHERE id = $4 RETURNING `+jobCols, worker, now.Add(lease), now, id))
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	})
	return out, err
}

func leased(n int64, err error, id, worker string) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return conflict("job %s is not leased by %s", id, worker)
	}
	return nil
}

// Heartbeat extends a lease held by worker.
func (s *Store) Heartbeat(ctx context.Context, id, worker string, lease time.Duration, cp json.RawMessage) error {
	now := utc(s.now())
	var cpArg any
	if cp != nil {
		cpArg = []byte(cp)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET lease_until = $1, updated_at = $2, checkpoint = COALESCE($3, checkpoint)
		WHERE id = $4 AND state = 'leased' AND worker = $5`, now.Add(lease), now, cpArg, id, worker)
	return leased(tag.RowsAffected(), err, id, worker)
}

// Finish completes, retries or kills a job leased by worker.
func (s *Store) Finish(ctx context.Context, id, worker string, o store.Outcome) error {
	now := utc(s.now())
	switch {
	case o.Done:
		tag, err := s.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1 AND state = 'leased' AND worker = $2`, id, worker)
		return leased(tag.RowsAffected(), err, id, worker)
	case o.Dead:
		tag, err := s.pool.Exec(ctx, `UPDATE jobs SET state = 'dead', last_error = $1, worker = '', updated_at = $2 WHERE id = $3 AND state = 'leased' AND worker = $4`,
			o.Error, now, id, worker)
		return leased(tag.RowsAffected(), err, id, worker)
	}
	// Retry: merge a pending duplicate (same key, enqueued meanwhile) into it.
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		notBefore := utc(o.RetryAt)
		var other time.Time
		err := tx.QueryRow(ctx, `DELETE FROM jobs WHERE state = 'pending' AND key <> '' AND (tenant, kind, key) =
			(SELECT tenant, kind, key FROM jobs WHERE id = $1 AND state = 'leased' AND worker = $2) RETURNING not_before`, id, worker).Scan(&other)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if other.After(notBefore) {
			notBefore = other
		}
		tag, err := tx.Exec(ctx, `UPDATE jobs SET state = 'pending', last_error = $1, worker = '', not_before = $2, updated_at = $3 WHERE id = $4 AND state = 'leased' AND worker = $5`,
			o.Error, notBefore, now, id, worker)
		return leased(tag.RowsAffected(), err, id, worker)
	})
}

// Jobs lists jobs, oldest first.
func (s *Store) Jobs(ctx context.Context, jq store.JobQuery) ([]store.Job, error) {
	limit := jq.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE ($1 = '' OR tenant = $1) AND ($2 = '' OR state = $2) AND ($3 = '' OR kind = $3) AND ($5 = '' OR key = $5)
		ORDER BY id LIMIT $4`, jq.Tenant, string(jq.State), jq.Kind, limit, jq.Key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
