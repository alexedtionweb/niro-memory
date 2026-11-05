package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

const jobCols = `id, tenant, kind, key, payload, state, attempts, not_before, lease_until, worker, checkpoint, last_error, created_at, updated_at, priority`

func scanJob(r pgx.Row) (store.Job, error) {
	var j store.Job
	var state string
	var lease *time.Time
	var payload, cp []byte
	err := r.Scan(&j.ID, &j.Tenant, &j.Kind, &j.Key, &payload, &state, &j.Attempts, &j.NotBefore, &lease, &j.Worker, &cp, &j.LastError, &j.CreatedAt, &j.UpdatedAt, &j.Priority)
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
	if n <= 0 {
		return nil, nil
	}
	var out []store.Job
	now := utc(s.now())
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH ranked AS (
			SELECT j.id, j.tenant, row_number() OVER (PARTITION BY j.tenant ORDER BY j.priority DESC, j.not_before, j.id) AS rn
			FROM jobs j
			WHERE ((j.state = 'pending' AND j.not_before <= $1) OR (j.state = 'leased' AND j.lease_until <= $1))
			AND (j.kind = 'tenant.purge' OR NOT EXISTS (SELECT 1 FROM tenant_purges p WHERE p.tenant = j.tenant AND p.state <> 'complete'))
			AND (j.key = '' OR NOT EXISTS (SELECT 1 FROM jobs b WHERE b.tenant = j.tenant AND b.kind = j.kind AND b.key = j.key
				AND b.state = 'leased' AND b.lease_until > $1))
			ORDER BY rn, j.tenant LIMIT $2)
		SELECT j.id, j.tenant, j.kind, j.key FROM jobs j JOIN ranked r ON r.id = j.id
		WHERE (j.key = '' OR pg_try_advisory_xact_lock(hashtextextended(j.tenant || '/' || j.kind || '/' || j.key, 0)))
		AND ((j.state = 'pending' AND j.not_before <= $1) OR (j.state = 'leased' AND j.lease_until <= $1))
		AND (j.kind = 'tenant.purge' OR NOT EXISTS (SELECT 1 FROM tenant_purges p WHERE p.tenant = j.tenant AND p.state <> 'complete'))
		ORDER BY r.rn, j.tenant
		FOR UPDATE OF j SKIP LOCKED`, now, n*4)
		if err != nil {
			return err
		}
		type candidate struct{ id, tenant, kind string }
		var candidates []candidate
		taken := map[string]bool{}
		for rows.Next() {
			var id, tenant, kind, key string
			if err := rows.Scan(&id, &tenant, &kind, &key); err != nil {
				rows.Close()
				return err
			}
			k := tenant + "\x00" + kind + "\x00" + key
			if len(candidates) == n || (key != "" && taken[k]) {
				continue
			}
			taken[k] = true
			candidates = append(candidates, candidate{id, tenant, kind})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, candidate := range candidates {
			// BeginPurge holds this tenant lock before reading/revoking leases.
			// Never wait here: our row locks could otherwise deadlock its revocation.
			var locked bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, "niromem.write:"+candidate.tenant).Scan(&locked); err != nil {
				return err
			}
			if !locked {
				continue
			}
			if candidate.kind != store.JobTenantPurge {
				// A separate statement gets a fresh READ COMMITTED snapshot after
				// acquiring the fence lock; the candidate query may predate a purge.
				var fenced bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_purges WHERE tenant = $1 AND state <> 'complete')`, candidate.tenant).Scan(&fenced); err != nil {
					return err
				}
				if fenced {
					continue
				}
			}
			j, err := scanJob(tx.QueryRow(ctx, `UPDATE jobs SET state = 'leased', worker = $1, lease_until = $2, attempts = attempts + 1, updated_at = $3
				WHERE id = $4 RETURNING `+jobCols, worker, now.Add(lease), now, candidate.id))
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
		tag, err := s.pool.Exec(ctx, `UPDATE jobs SET state = 'dead', last_error = $1, worker = '', lease_until = NULL, updated_at = $2 WHERE id = $3 AND state = 'leased' AND worker = $4`,
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
		yield := 0
		if o.Yield {
			yield = 1
		}
		tag, err := tx.Exec(ctx, `UPDATE jobs SET state = 'pending', last_error = $1, worker = '', lease_until = NULL, not_before = $2, updated_at = $3, attempts = GREATEST(0, attempts - $6) WHERE id = $4 AND state = 'leased' AND worker = $5`,
			o.Error, notBefore, now, id, worker, yield)
		return leased(tag.RowsAffected(), err, id, worker)
	})
}

// Jobs lists jobs, oldest first.
func (s *Store) Jobs(ctx context.Context, jq store.JobQuery) ([]store.Job, error) {
	limit := jq.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	var conds []string
	var args []any
	for col, value := range map[string]string{"tenant": jq.Tenant, "id": jq.ID, "state": string(jq.State), "kind": jq.Kind, "key": jq.Key} {
		if value != "" {
			args = append(args, value)
			conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	if jq.AfterID != "" {
		args = append(args, jq.AfterID)
		conds = append(conds, fmt.Sprintf("id > $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM jobs`+where+fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args)), args...)
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

// JobControl locks and updates exactly one tenant-scoped job.
func (s *Store) JobControl(ctx context.Context, c store.JobControl) (out store.Job, err error) {
	if err := c.Validate(); err != nil {
		return out, err
	}
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "niromem.write:"+c.Tenant); err != nil {
			return err
		}
		current, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE tenant = $1 AND id = $2 FOR UPDATE`, c.Tenant, c.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return memory.ErrNotFound
		}
		if err != nil {
			return err
		}
		out, err = store.ApplyJobControl(current, c, utc(s.now()))
		if err != nil {
			return err
		}
		if out.State == store.JobPending && out.Key != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE tenant = $1 AND kind = $2 AND key = $3 AND state = 'pending' AND id <> $4)`,
				out.Tenant, out.Kind, out.Key, out.ID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return conflict("pending job already owns key %s", out.Key)
			}
		}
		if c.Action == store.JobDelete {
			_, err = tx.Exec(ctx, `DELETE FROM jobs WHERE tenant = $1 AND id = $2`, c.Tenant, c.ID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE jobs SET state = $1, attempts = $2, priority = $3, not_before = $4, lease_until = NULL, worker = $5, checkpoint = $6, last_error = $7, updated_at = $8 WHERE tenant = $9 AND id = $10`,
				out.State, out.Attempts, out.Priority, out.NotBefore, out.Worker, []byte(out.Checkpoint), out.LastError, out.UpdatedAt, c.Tenant, c.ID)
		}
		return err
	})
	if isUnique(err) {
		err = conflict("pending job already owns key %s", out.Key)
	}
	if err != nil {
		return store.Job{}, err
	}
	return out, nil
}
