package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

const jobCols = `id, tenant, kind, key, payload, state, attempts, not_before, lease_until, worker, checkpoint, last_error, created_at, updated_at, priority`

func scanJob(r scanner) (store.Job, error) {
	var j store.Job
	var nb, lu, ca, ua int64
	var payload, cp []byte
	err := r.Scan(&j.ID, &j.Tenant, &j.Kind, &j.Key, &payload, &j.State, &j.Attempts, &nb, &lu, &j.Worker, &cp, &j.LastError, &ca, &ua, &j.Priority)
	j.NotBefore, j.CreatedAt, j.UpdatedAt = fromMs(nb), fromMs(ca), fromMs(ua)
	if lu > 0 {
		j.LeaseUntil = fromMs(lu)
	}
	if len(payload) > 0 {
		j.Payload = json.RawMessage(payload)
	}
	if len(cp) > 0 {
		j.Checkpoint = json.RawMessage(cp)
	}
	return j, err
}

// Claim leases due jobs: pending jobs whose NotBefore has passed and leased
// jobs whose lease expired. Candidates are ranked per tenant (round-robin) and
// a key that holds a live lease is skipped.
func (s *Store) Claim(ctx context.Context, worker string, n int, lease time.Duration) (out []store.Job, err error) {
	if n <= 0 {
		return nil, nil
	}
	now := ms(s.now())
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `SELECT id, tenant, kind, key FROM (
		SELECT j.id, j.tenant, j.kind, j.key, j.not_before,
			ROW_NUMBER() OVER (PARTITION BY j.tenant ORDER BY j.priority DESC, j.not_before, j.id) AS rn
		FROM jobs j
		WHERE ((j.state = 'pending' AND j.not_before <= ?1) OR (j.state = 'leased' AND j.lease_until <= ?1))
		AND (j.kind = 'tenant.purge' OR NOT EXISTS (SELECT 1 FROM tenant_purges p WHERE p.tenant = j.tenant AND p.state <> 'complete'))
		AND (j.key = '' OR NOT EXISTS (SELECT 1 FROM jobs b WHERE b.tenant = j.tenant AND b.kind = j.kind AND b.key = j.key
			AND b.state = 'leased' AND b.lease_until > ?1))
	) ORDER BY rn, tenant, not_before LIMIT ?2`, now, n*4)
	if err != nil {
		return nil, err
	}
	var ids []string
	taken := map[string]bool{}
	for rows.Next() && len(ids) < n {
		var id, tenant, kind, key string
		if err := rows.Scan(&id, &tenant, &kind, &key); err != nil {
			rows.Close()
			return nil, err
		}
		k := tenant + "\x00" + kind + "\x00" + key
		if key != "" && taken[k] {
			continue
		}
		taken[k] = true
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		j, err := scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET state = 'leased', worker = ?, lease_until = ?, attempts = attempts + 1, updated_at = ?
			WHERE id = ? RETURNING `+jobCols, worker, now+lease.Milliseconds(), now, id))
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, tx.Commit()
}

// Heartbeat extends a lease held by worker.
func (s *Store) Heartbeat(ctx context.Context, id, worker string, lease time.Duration, cp json.RawMessage) error {
	now := ms(s.now())
	r, err := s.w.ExecContext(ctx, `UPDATE jobs SET lease_until = ?, updated_at = ?, checkpoint = COALESCE(?, checkpoint)
		WHERE id = ? AND state = 'leased' AND worker = ?`, now+lease.Milliseconds(), now, nullBytes(cp), id, worker)
	return leaseResult(r, err, id, worker)
}

func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

func leaseResult(r sql.Result, err error, id, worker string) error {
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return conflict("job %s is not leased by %s", id, worker)
	}
	return nil
}

// Finish completes, retries or kills a job leased by worker.
func (s *Store) Finish(ctx context.Context, id, worker string, o store.Outcome) error {
	now := ms(s.now())
	var r sql.Result
	var err error
	switch {
	case o.Done:
		r, err = s.w.ExecContext(ctx, `DELETE FROM jobs WHERE id = ? AND state = 'leased' AND worker = ?`, id, worker)
	case o.Dead:
		r, err = s.w.ExecContext(ctx, `UPDATE jobs SET state = 'dead', last_error = ?, worker = '', lease_until = 0, updated_at = ? WHERE id = ? AND state = 'leased' AND worker = ?`,
			o.Error, now, id, worker)
	default:
		return s.retry(ctx, id, worker, o, now)
	}
	return leaseResult(r, err, id, worker)
}

// retry makes a leased job pending again. A pending job with the same key
// (enqueued while this one ran) is merged into it: the retried job keeps its
// checkpoint and attempts and takes the later NotBefore.
func (s *Store) retry(ctx context.Context, id, worker string, o store.Outcome, now int64) (err error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	notBefore := ms(o.RetryAt)
	var other int64
	err = tx.QueryRowContext(ctx, `DELETE FROM jobs WHERE state = 'pending' AND key <> '' AND (tenant, kind, key) =
		(SELECT tenant, kind, key FROM jobs WHERE id = ? AND state = 'leased' AND worker = ?) RETURNING not_before`, id, worker).Scan(&other)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if other > notBefore {
		notBefore = other
	}
	yield := 0
	if o.Yield {
		yield = 1
	}
	r, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'pending', last_error = ?, worker = '', lease_until = 0, not_before = ?, updated_at = ?, attempts = MAX(0, attempts - ?) WHERE id = ? AND state = 'leased' AND worker = ?`,
		o.Error, notBefore, now, yield, id, worker)
	if err = leaseResult(r, err, id, worker); err != nil {
		return err
	}
	return tx.Commit()
}

// Jobs lists jobs, oldest first.
func (s *Store) Jobs(ctx context.Context, q store.JobQuery) ([]store.Job, error) {
	var conds []string
	var args []any
	for col, v := range map[string]string{"id": q.ID, "tenant": q.Tenant, "state": string(q.State), "kind": q.Kind, "key": q.Key} {
		if v != "" {
			conds, args = append(conds, col+" = ?"), append(args, v)
		}
		if q.AfterID != "" {
			conds, args = append(conds, "id > ?"), append(args, q.AfterID)
		}
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs`+where+` ORDER BY id`+limitOffset(q.Limit, 0), args...)
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

func migrateJobPriority(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `SELECT priority FROM jobs LIMIT 0`); err != nil {
		if _, err := db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100)`); err != nil {
			// Another opener may have added the column while this one waited.
			if _, check := db.ExecContext(ctx, `SELECT priority FROM jobs LIMIT 0`); check != nil {
				return err
			}
		}
	}
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS jobs_priority_due ON jobs (state, priority DESC, not_before, tenant)`)
	return err
}

// JobControl applies a lifecycle change to one tenant-scoped job.
func (s *Store) JobControl(ctx context.Context, c store.JobControl) (out store.Job, err error) {
	if err := c.Validate(); err != nil {
		return out, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE tenant = ? AND id = ?`, c.Tenant, c.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return out, memory.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out, err = store.ApplyJobControl(current, c, fromMs(ms(s.now())))
	if err != nil {
		return store.Job{}, err
	}
	if out.State == store.JobPending && out.Key != "" {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE tenant = ? AND kind = ? AND key = ? AND state = 'pending' AND id <> ?)`,
			out.Tenant, out.Kind, out.Key, out.ID).Scan(&exists); err != nil {
			return store.Job{}, err
		}
		if exists {
			return store.Job{}, conflict("pending job already owns key %s", out.Key)
		}
	}
	if c.Action == store.JobDelete {
		_, err = tx.ExecContext(ctx, `DELETE FROM jobs WHERE tenant = ? AND id = ?`, c.Tenant, c.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = ?, attempts = ?, priority = ?, not_before = ?, lease_until = ?, worker = ?, checkpoint = ?, last_error = ?, updated_at = ? WHERE tenant = ? AND id = ?`,
			out.State, out.Attempts, out.Priority, ms(out.NotBefore), int64(0), out.Worker, nullBytes(out.Checkpoint), out.LastError, ms(out.UpdatedAt), c.Tenant, c.ID)
	}
	if err != nil {
		return store.Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Job{}, err
	}
	return out, nil
}
