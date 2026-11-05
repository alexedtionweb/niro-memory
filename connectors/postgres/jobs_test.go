package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/jackc/pgx/v5"
)

func TestClaimSkipsTenantWriteLock(t *testing.T) {
	s := atomicStore(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "job", Kind: "work"}}}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "niromem.write:t"); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	claimed, err := s.Claim(claimCtx, "w", 1, time.Minute)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("claim must skip held tenant lock without waiting: %+v, %v", claimed, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.Claim(ctx, "w", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim after lock release: %+v, %v", claimed, err)
	}
}

func TestClaimBeginPurgeRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		s := atomicStore(t)
		ctx := context.Background()
		if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{{ID: "job", Kind: "work"}}}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var claimed []store.Job
		var claimErr, purgeErr error
		var purge *store.PurgeStatus
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			claimed, claimErr = s.Claim(ctx, "w", 1, time.Minute)
		}()
		go func() {
			defer wg.Done()
			<-start
			purge, purgeErr = s.BeginPurge(ctx, "t", "purge")
		}()
		close(start)
		wg.Wait()
		if claimErr != nil || purgeErr != nil {
			t.Fatalf("claim/purge: %v, %v", claimErr, purgeErr)
		}
		for _, j := range claimed {
			if j.Kind == store.JobTenantPurge {
				continue
			}
			if purge.ReadyAt.Before(j.LeaseUntil) {
				t.Fatalf("purge missed concurrent lease: ready %v, lease %v", purge.ReadyAt, j.LeaseUntil)
			}
			if err := s.Heartbeat(ctx, j.ID, j.Worker, time.Minute, nil); !errors.Is(err, memory.ErrConflict) {
				t.Fatalf("purge failed to revoke concurrent lease: %v", err)
			}
		}
		left, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "job"})
		if err != nil || len(left) != 1 || left[0].State == store.JobLeased {
			t.Fatalf("ordinary lease survived purge: %+v, %v", left, err)
		}
	}
}
func TestJobPriorityMigration(t *testing.T) {
	dsn := os.Getenv("NIROMEM_PG_DSN")
	if dsn == "" {
		t.Skip("NIROMEM_PG_DSN not set")
	}
	ctx := context.Background()
	raw, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close(ctx)
	name := "job_migration_" + strings.ReplaceAll(memory.NewID(), "-", "")
	if _, err := raw.Exec(ctx, `CREATE SCHEMA `+name+`; SET search_path TO `+name); err != nil {
		t.Fatal(err)
	}
	defer raw.Exec(ctx, `DROP SCHEMA `+name+` CASCADE`)
	start := strings.Index(schema, "CREATE TABLE IF NOT EXISTS jobs (")
	legacy := schema[start : start+strings.Index(schema[start:], "CREATE UNIQUE INDEX")]
	legacy = strings.Replace(legacy, "    priority    int         NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100),\n", "", 1)
	if _, err := raw.Exec(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(ctx, `INSERT INTO jobs(id,tenant,kind,state,not_before,created_at,updated_at) VALUES('legacy','t','work','pending',now(),now(),now())`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, Options{DSN: dsn, Schema: name})
	if err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "legacy"})
	if err != nil || len(js) != 1 || js[0].Priority != 0 {
		t.Fatalf("migrated default: %+v, %v", js, err)
	}
	if _, err := s.JobControl(ctx, store.JobControl{Tenant: "t", ID: "legacy", Action: store.JobSetPriority, Priority: -75}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, Options{DSN: dsn, Schema: name})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	js, err = s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "legacy"})
	if err != nil || len(js) != 1 || js[0].Priority != -75 {
		t.Fatalf("reopened priority: %+v, %v", js, err)
	}
	if _, err := raw.Exec(ctx, `UPDATE jobs SET priority = -101 WHERE id = 'legacy'`); err == nil {
		t.Fatal("migrated priority constraint missing")
	}
}
