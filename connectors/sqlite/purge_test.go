package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/connectors/sqlite"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func TestPurgeResumesAfterRestartWithBoundedBatches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "purge.db")
	openDB := func() *sqlite.Store {
		t.Helper()
		s, err := sqlite.Open(ctx, sqlite.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := openDB()
	b := &store.Batch{Tenant: "wipe", Settings: []byte(`{}`)}
	for i := 0; i < 11; i++ {
		b.Jobs = append(b.Jobs, store.Job{Kind: "test", Key: memory.NewID()})
	}
	if _, err := s.Write(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPurge(ctx, "wipe", "request"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PurgeBatch(ctx, "wipe", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openDB()
	defer s.Close()
	if _, err := s.Write(ctx, &store.Batch{Tenant: "wipe", Settings: []byte(`{}`)}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("restart lost fence %v", err)
	}
	var prior int64
	done := false
	for i := 0; i < 100 && !done; i++ {
		p, finished, err := s.PurgeBatch(ctx, "wipe", 2)
		if err != nil {
			t.Fatal(err)
		}
		if p.Deleted-prior > 2 {
			t.Fatalf("unbounded deletion: %d", p.Deleted-prior)
		}
		prior = p.Deleted
		done = finished
	}
	if !done {
		t.Fatal("did not complete")
	}
	if err := s.FinishPurge(ctx, "wipe"); err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "wipe"})
	if err != nil || len(js) != 1 || js[0].Kind != store.JobTenantPurge {
		t.Fatalf("stale jobs %+v %v", js, err)
	}
}

func TestPurgeWaitsForRevokedLeaseAndPreventsOldWorkerFinish(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "drain.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write(ctx, &store.Batch{Tenant: "wipe", Jobs: []store.Job{{Kind: "slow"}}}); err != nil {
		t.Fatal(err)
	}
	js, err := s.Claim(ctx, "old", 1, 30*time.Second)
	if err != nil || len(js) != 1 {
		t.Fatalf("claim %+v %v", js, err)
	}
	p, err := s.BeginPurge(ctx, "wipe", "request")
	if err != nil || !p.ReadyAt.After(now) {
		t.Fatalf("drain %+v %v", p, err)
	}
	if _, _, err := s.PurgeBatch(ctx, "wipe", 2); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("did not wait %v", err)
	}
	if err := s.Heartbeat(ctx, js[0].ID, "old", time.Minute, nil); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("old heartbeat accepted %v", err)
	}
	if err := s.Finish(ctx, js[0].ID, "old", store.Outcome{Done: true}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("old finish accepted %v", err)
	}
	now = p.ReadyAt.Add(time.Second)
	if _, _, err := s.PurgeBatch(ctx, "wipe", 2); err != nil {
		t.Fatal(err)
	}
}
