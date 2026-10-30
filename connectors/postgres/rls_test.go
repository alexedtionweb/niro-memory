package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/alexedtionweb/niro-memory/connectors/postgres"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/jackc/pgx/v5"
)

// TestRLSEnforced proves the policies hold for a non-superuser owner role.
// It needs a superuser DSN to create that role:
//
//	NIROMEM_PG_ADMIN_DSN=postgres://postgres@localhost:5432/postgres?sslmode=disable
func TestRLSEnforced(t *testing.T) {
	ctx := context.Background()
	admin := os.Getenv("NIROMEM_PG_ADMIN_DSN")
	if admin == "" {
		t.Skip("NIROMEM_PG_ADMIN_DSN not set")
	}
	su, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	su.Exec(ctx, `DROP SCHEMA IF EXISTS rlsprobe CASCADE`)
	su.Exec(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app') THEN CREATE ROLE app LOGIN NOSUPERUSER NOBYPASSRLS; END IF; END $$`)
	su.Exec(ctx, `GRANT CREATE ON DATABASE postgres TO app`)
	su.Close(ctx)
	cfg, _ := pgx.ParseConfig(admin)
	dsn := fmt.Sprintf("postgres://app@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, cfg.Database)
	s, err := postgres.Open(ctx, postgres.Options{DSN: dsn, Schema: "rlsprobe", RLS: true})
	if err != nil {
		t.Fatal(err)
	}
	m := memory.Memory{ID: memory.NewID(), Scope: memory.Scope{Tenant: "a"}, Kind: "semantic", Layer: 1, Text: "secret of a", Visibility: "team"}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "a", Memories: []memory.Memory{m}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "a", []string{m.ID}); len(got) != 1 {
		t.Fatal("own tenant read failed")
	}
	raw, _ := pgx.Connect(ctx, dsn+"&search_path=rlsprobe")
	var n int
	if err := raw.QueryRow(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("without a tenant setting RLS must hide rows: n=%d err=%v", n, err)
	}
	tx, _ := raw.Begin(ctx)
	tx.Exec(ctx, `SELECT set_config('niromem.tenant', 'b', true)`)
	tx.QueryRow(ctx, `SELECT count(*) FROM memories`).Scan(&n)
	if n != 0 {
		t.Fatal("tenant b sees tenant a's rows")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memories (tenant, id, kind, layer, text, visibility, valid_from, recorded_at) VALUES ('a','x','semantic',1,'forged','team',now(),now())`); err == nil {
		t.Fatal("tenant b wrote into tenant a")
	}
	tx.Rollback(ctx)
}
