package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/jackc/pgx/v5"
)

var purgeTables = []string{"memory_entities", "entity_aliases", "knowledge_relations", "relationship_sets", "history", "embeddings", "memories", "episodes", "runs", "entities", "operation_receipts", "settings", "jobs"}

const purgeCols = "id,tenant,state,phase,deleted,ready_at"

func scanPurge(row pgx.Row) (*store.PurgeStatus, error) {
	p := &store.PurgeStatus{}
	err := row.Scan(&p.ID, &p.Tenant, &p.State, &p.Phase, &p.Deleted, &p.ReadyAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = memory.ErrNotFound
	}
	return p, err
}

func (s *Store) purgeTx(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('niromem.tenant',$1,true)`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "niromem.write:"+tenant); err != nil {
			return err
		}
		return fn(tx)
	})
}

// BeginPurge atomically fences writes, revokes jobs and schedules cleanup.
func (s *Store) BeginPurge(ctx context.Context, tenant, id string) (*store.PurgeStatus, error) {
	if tenant == "" || id == "" {
		return nil, fmt.Errorf("%w: tenant and purge id required", memory.ErrInvalid)
	}
	var p *store.PurgeStatus
	err := s.purgeTx(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		p, err = scanPurge(tx.QueryRow(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=$1`, tenant))
		if err != nil && !errors.Is(err, memory.ErrNotFound) {
			return err
		}
		var prior string
		requestErr := tx.QueryRow(ctx, `SELECT purge_id FROM tenant_purge_requests WHERE tenant=$1 AND request_id=$2`, tenant, id).Scan(&prior)
		if requestErr == nil {
			if p.ID != prior {
				p = &store.PurgeStatus{ID: prior, Tenant: tenant, State: "complete"}
			}
			return nil
		}
		if !errors.Is(requestErr, pgx.ErrNoRows) {
			return requestErr
		}
		if err == nil && p.State != "complete" {
			_, err = tx.Exec(ctx, `INSERT INTO tenant_purge_requests(tenant,request_id,purge_id) VALUES($1,$2,$3)`, tenant, id, p.ID)
			return err
		}
		now := utc(s.now())
		var ready time.Time
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(lease_until),$2) FROM jobs WHERE tenant=$1 AND state='leased'`, tenant, now).Scan(&ready); err != nil {
			return err
		}
		if ready.After(now) {
			ready = ready.Add(time.Second)
		} else {
			ready = now
		}
		if _, err = tx.Exec(ctx, `INSERT INTO tenant_purges(tenant,id,state,phase,deleted,ready_at) VALUES($1,$2,'purging',0,0,$3)
			ON CONFLICT(tenant) DO UPDATE SET id=excluded.id,state='purging',phase=0,deleted=0,ready_at=excluded.ready_at`, tenant, id, ready); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO tenant_purge_requests(tenant,request_id,purge_id) VALUES($1,$2,$2)`, tenant, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='cancelled',worker='',updated_at=$2 WHERE tenant=$1 AND state='leased' AND kind<>$3`, tenant, now, store.JobTenantPurge); err != nil {
			return err
		}
		if _, err = (&writer{tx: tx, ctx: ctx, t: tenant, now: now}).apply(&store.Batch{Tenant: tenant, Jobs: []store.Job{{Kind: store.JobTenantPurge, Key: id, NotBefore: ready}}}); err != nil {
			return err
		}
		p, err = scanPurge(tx.QueryRow(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=$1`, tenant))
		return err
	})
	return p, err
}

// PurgeStatus returns the last wipe's progress.
func (s *Store) PurgeStatus(ctx context.Context, tenant string) (*store.PurgeStatus, error) {
	var p *store.PurgeStatus
	err := s.read(ctx, tenant, func(q q) error {
		var err error
		p, err = scanPurge(q.QueryRow(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=$1`, tenant))
		return err
	})
	return p, err
}

// PurgeBatch deletes a bounded set of tuple identifiers in one short transaction.
func (s *Store) PurgeBatch(ctx context.Context, tenant string, limit int) (*store.PurgeStatus, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, fmt.Errorf("%w: purge batch must be 1..1000", memory.ErrInvalid)
	}
	var p *store.PurgeStatus
	done := false
	err := s.purgeTx(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		p, err = scanPurge(tx.QueryRow(ctx, `SELECT `+purgeCols+` FROM tenant_purges WHERE tenant=$1 FOR UPDATE`, tenant))
		if err != nil {
			return err
		}
		if p.State == "complete" || p.Phase >= len(purgeTables) {
			done = true
			return nil
		}
		if s.now().Before(p.ReadyAt) {
			return conflict("waiting for revoked worker leases to expire")
		}
		table := purgeTables[p.Phase]
		extra := ""
		if table == "jobs" {
			extra = " AND kind<>'tenant.purge'"
		}
		r, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE ctid IN (SELECT ctid FROM `+table+` WHERE tenant=$1`+extra+` LIMIT $2)`, tenant, limit)
		if err != nil {
			return err
		}
		p.Deleted += r.RowsAffected()
		if r.RowsAffected() == 0 {
			p.Phase++
		}
		_, err = tx.Exec(ctx, `UPDATE tenant_purges SET phase=$2,deleted=$3 WHERE tenant=$1`, tenant, p.Phase, p.Deleted)
		done = p.Phase >= len(purgeTables)
		return err
	})
	return p, done, err
}

// FinishPurge unlocks the empty tenant.
func (s *Store) FinishPurge(ctx context.Context, tenant string) error {
	return s.purgeTx(ctx, tenant, func(tx pgx.Tx) error {
		r, err := tx.Exec(ctx, `UPDATE tenant_purges SET state='complete' WHERE tenant=$1 AND phase>=$2`, tenant, len(purgeTables))
		if err == nil && r.RowsAffected() == 0 {
			return conflict("purge has unfinished tables")
		}
		return err
	})
}
