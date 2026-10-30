package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/alexedtionweb/niro-memory/connectors/sqlite"
	"github.com/alexedtionweb/niro-memory/enginetest"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/storetest"
	"github.com/alexedtionweb/niro-memory/vector"
	"github.com/alexedtionweb/niro-memory/vector/vectortest"
)

func open(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{Path: filepath.Join(t.TempDir(), "mem.db")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}

func TestSQLConsoleIsReadOnly(t *testing.T) {
	s := open(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Settings: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	cols, rows, err := s.QueryReadOnly(ctx, "SELECT tenant, doc FROM settings", 10)
	if err != nil || len(cols) != 2 || len(rows) != 1 || rows[0][0] != "t" {
		t.Fatalf("select: %v %v %v", cols, rows, err)
	}
	for _, q := range []string{
		"DELETE FROM settings",
		"INSERT INTO settings VALUES ('x', 'y')",
		"DROP TABLE settings",
		"PRAGMA query_only=0",
	} {
		_, _, err := s.QueryReadOnly(ctx, q, 10)
		_, rows, _ := s.QueryReadOnly(ctx, "SELECT count(*) FROM settings", 10)
		if rows[0][0].(int64) != 1 {
			t.Fatalf("%q modified the database (err=%v)", q, err)
		}
	}
}

func TestEngine(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}

func TestVectorIndex(t *testing.T) {
	vectortest.Run(t, func(t *testing.T) vector.Index { return open(t) })
}
