package sqlite

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/storetest"
)

func atomicPath(t *testing.T) string {
	t.Helper()
	path := ".atomic-" + memory.NewID() + ".db"
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
	})
	return path
}

func TestAtomicStorage(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := Open(context.Background(), Options{Path: atomicPath(t)})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestOperationAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := atomicPath(t)
	a, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	batch := &store.Batch{Tenant: "t", OperationKey: "same", OperationHash: "hash", Result: []byte(`{"ok":true}`)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	replays := 0
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			r, err := s.Write(ctx, batch)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Replayed {
				replays++
			}
		}(s)
	}
	wg.Wait()
	if replays != 1 {
		t.Fatalf("replays=%d", replays)
	}
	if r, err := b.Operation(ctx, "t", "same"); err != nil || string(r.Result) != `{"ok":true}` {
		t.Fatalf("receipt %+v %v", r, err)
	}
}

func TestLegacyClosureMigration(t *testing.T) {
	ctx := context.Background()
	path := atomicPath(t)
	s, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE memories DROP COLUMN closed_at`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()
	s, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.w.ExecContext(ctx, `SELECT closed_at FROM memories LIMIT 0`); err != nil {
		t.Fatalf("legacy migration: %v", err)
	}
}
