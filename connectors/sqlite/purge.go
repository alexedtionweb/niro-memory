package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

var purgeTables = []struct{ table, key string }{
	{"memory_entities", "tenant, entity_id, memory_id"}, {"entity_aliases", "tenant, alias"},
	{"knowledge_relations", "tenant, id"}, {"relationship_sets", "tenant, anchor"},
	{"history", "rowid"}, {"embeddings", "tenant, model, target_key"},
	{"memories", "rowid"}, {"episodes", "rowid"}, {"runs", "tenant, run_id"},
	{"entities", "tenant, key"}, {"operation_receipts", "tenant, key"}, {"settings", "tenant"},
	{"jobs", "id"},
}

func scanPurge(r scanner) (*store.PurgeStatus, error) {
	p := &store.PurgeStatus{}
	var ready int64
	err := r.Scan(&p.ID, &p.Tenant, &p.State, &p.Phase, &p.Deleted, &ready)
	p.ReadyAt = fromMs(ready)
	if errors.Is(err, sql.ErrNoRows) {
		err = memory.ErrNotFound
	}
	return p, err
}

const purgeCols = "id, tenant, state, phase, deleted, ready_at"

// BeginPurge fences this tenant and schedules durable cleanup.
func (s *Store) BeginPurge(ctx context.Context, tenant, id string) (p *store.PurgeStatus, err error) {
	if tenant == "" || id == "" {
		return nil, fmt.Errorf("%w: tenant and purge id required", memory.ErrInvalid)
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err = scanPurge(tx.QueryRowContext(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant = ?`, tenant))
	if err != nil && !errors.Is(err, memory.ErrNotFound) {
		return nil, err
	}
	var prior string
	requestErr := tx.QueryRowContext(ctx, `SELECT purge_id FROM tenant_purge_requests WHERE tenant=? AND request_id=?`, tenant, id).Scan(&prior)
	if requestErr == nil {
		if p.ID != prior {
			p = &store.PurgeStatus{ID: prior, Tenant: tenant, State: "complete"}
		}
		return p, tx.Commit()
	}
	if !errors.Is(requestErr, sql.ErrNoRows) {
		return nil, requestErr
	}
	if err == nil && p.State != "complete" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO tenant_purge_requests(tenant,request_id,purge_id) VALUES(?,?,?)`, tenant, id, p.ID); err != nil {
			return nil, err
		}
		return p, tx.Commit()
	}
	now := s.now()
	var last int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(lease_until),0) FROM jobs WHERE tenant=? AND state='leased'`, tenant).Scan(&last); err != nil {
		return nil, err
	}
	ready := now
	if last > ms(now) {
		ready = fromMs(last).Add(time.Second)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tenant_purges(tenant,id,state,phase,deleted,ready_at) VALUES(?,?,'purging',0,0,?)
		ON CONFLICT(tenant) DO UPDATE SET id=excluded.id,state='purging',phase=0,deleted=0,ready_at=excluded.ready_at`, tenant, id, ms(ready)); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tenant_purge_requests(tenant,request_id,purge_id) VALUES(?,?,?)`, tenant, id, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='cancelled', worker='', updated_at=? WHERE tenant=? AND state='leased' AND kind<>?`, ms(now), tenant, store.JobTenantPurge); err != nil {
		return nil, err
	}
	w := &writer{tx: tx, ctx: ctx, t: tenant, now: now}
	if _, err = w.apply(&store.Batch{Tenant: tenant, Jobs: []store.Job{{Kind: store.JobTenantPurge, Key: id, NotBefore: ready}}}); err != nil {
		return nil, err
	}
	p, err = scanPurge(tx.QueryRowContext(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=?`, tenant))
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// PurgeStatus returns the last wipe's durable progress.
func (s *Store) PurgeStatus(ctx context.Context, tenant string) (*store.PurgeStatus, error) {
	return scanPurge(s.r.QueryRowContext(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=?`, tenant))
}

// PurgeBatch removes at most limit rows from one indexed tenant table.
func (s *Store) PurgeBatch(ctx context.Context, tenant string, limit int) (*store.PurgeStatus, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, fmt.Errorf("%w: purge batch must be 1..1000", memory.ErrInvalid)
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	p, err := scanPurge(tx.QueryRowContext(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=?`, tenant))
	if err != nil {
		return nil, false, err
	}
	if p.State == "complete" {
		return p, true, tx.Commit()
	}
	if s.now().Before(p.ReadyAt) {
		return nil, false, conflict("waiting for revoked worker leases to expire")
	}
	if p.Phase >= len(purgeTables) {
		return p, true, tx.Commit()
	}
	t := purgeTables[p.Phase]
	extra := ""
	if t.table == "jobs" {
		extra = " AND kind<>'tenant.purge'"
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM `+t.table+` WHERE (`+t.key+`) IN (SELECT `+t.key+` FROM `+t.table+` WHERE tenant=?`+extra+` LIMIT ?)`, tenant, limit)
	if err != nil {
		return nil, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	p.Deleted += n
	if n == 0 {
		p.Phase++
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tenant_purges SET phase=?,deleted=? WHERE tenant=?`, p.Phase, p.Deleted, tenant); err != nil {
		return nil, false, err
	}
	return p, p.Phase >= len(purgeTables), tx.Commit()
}

// FinishPurge unlocks an empty tenant after external projections were removed.
func (s *Store) FinishPurge(ctx context.Context, tenant string) error {
	r, err := s.w.ExecContext(ctx, `UPDATE tenant_purges SET state='complete' WHERE tenant=? AND phase>=?`, tenant, len(purgeTables))
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return conflict("purge has unfinished tables")
	}
	return err
}
