package postgres

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/storetest"
	"github.com/jackc/pgx/v5"
)

func atomicStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("NIROMEM_PG_DSN")
	if dsn == "" {
		t.Skip("NIROMEM_PG_DSN not set")
	}
	schema := "atomic_" + strings.ReplaceAll(memory.NewID(), "-", "")
	s, err := Open(context.Background(), Options{DSN: dsn, Schema: schema, RLS: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The conformance runner closes s before dropping its isolated schema.
		raw, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = raw.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
			raw.Close(context.Background())
		}
	})
	return s
}

func TestAtomicStorage(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return atomicStore(t) })
}

func TestOperationAcrossPools(t *testing.T) {
	ctx := context.Background()
	a := atomicStore(t)
	defer a.Close()
	cfg := a.pool.Config()
	b, err := Open(ctx, Options{DSN: os.Getenv("NIROMEM_PG_DSN"), Schema: cfg.ConnConfig.RuntimeParams["search_path"], RLS: true})
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
}

func TestAtomicRLSPolicies(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("NIROMEM_PG_ADMIN_DSN")
	if dsn == "" {
		t.Skip("NIROMEM_PG_ADMIN_DSN not set")
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	token := strings.ReplaceAll(memory.NewID(), "-", "")
	role, schema := "role_"+token, "rls_"+token
	password := memory.NewID()
	if _, err := admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '`+password+`'`); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP ROLE `+pgx.Identifier{role}.Sanitize())
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `GRANT CREATE ON DATABASE `+pgx.Identifier{cfg.Database}.Sanitize()+` TO `+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `REVOKE ALL ON DATABASE `+pgx.Identifier{cfg.Database}.Sanitize()+` FROM `+pgx.Identifier{role}.Sanitize())
	cfg.User, cfg.Password = role, password
	roleDSN := dsn + " user=" + role + " password=" + password
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, password)
		roleDSN = u.String()
	}
	s, err := Open(ctx, Options{DSN: roleDSN, Schema: schema, RLS: true})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	defer s.Close()
	a, b := memory.Memory{ID: "a"}, memory.Memory{ID: "b"}
	batch := &store.Batch{Tenant: "private", OperationKey: "k", OperationHash: "h", Result: []byte(`{"secret":true}`), Memories: []memory.Memory{a, b},
		Relations: []relations.Link{{ID: "r", Source: "a", Target: "b", Relation: "supports"}}}
	if _, err := s.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	raw, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close(ctx)
	for _, table := range []string{"knowledge_relations", "operation_receipts"} {
		var count int
		if err := raw.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s exposed unscoped rows: %d %v", table, count, err)
		}
	}
	if _, err := raw.Exec(ctx, `INSERT INTO operation_receipts(tenant,key,hash,receipt) VALUES('private','forged','h','{}')`); err == nil {
		t.Fatal("RLS allowed unscoped receipt write")
	}
}

func TestLegacyClosureMigration(t *testing.T) {
	ctx := context.Background()
	s := atomicStore(t)
	schema := s.pool.Config().ConnConfig.RuntimeParams["search_path"]
	m := memory.Memory{ID: "legacy", Scope: memory.Scope{Tenant: "t"}, Kind: "semantic", Layer: 1, Text: "old", Visibility: "team"}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Memories: []memory.Memory{m}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE memories DROP COLUMN closed_at; DROP TABLE knowledge_relations; DROP TABLE operation_receipts`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(ctx, Options{DSN: os.Getenv("NIROMEM_PG_DSN"), Schema: schema, RLS: true})
	if err != nil {
		t.Fatalf("legacy migration: %v", err)
	}
	defer s.Close()
	got, err := s.Get(ctx, "t", []string{"legacy"})
	if err != nil || len(got) != 1 || got[0].ClosedAt != nil {
		t.Fatalf("legacy row: %+v %v", got, err)
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", OperationKey: "k", OperationHash: "h", Relations: []relations.Link{{ID: "self", Source: "legacy", Target: "legacy", Relation: "related_to"}}}); err != nil {
		t.Fatalf("migrated tables unusable: %v", err)
	}
	if r, err := s.Operation(ctx, "t", "k"); err != nil || r.OperationHash != "h" {
		t.Fatalf("migrated receipt: %+v %v", r, err)
	}
}
