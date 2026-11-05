package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-memory/store"
)

func TestJobPriorityMigration(t *testing.T) {
	ctx := context.Background()
	path := atomicPath(t)
	if err := register(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(schema, "CREATE TABLE IF NOT EXISTS jobs (")
	legacy := schema[start : start+strings.Index(schema[start:], "CREATE UNIQUE INDEX")]
	legacy = strings.Replace(legacy, "    priority    INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100),\n", "", 1)
	if _, err := db.ExecContext(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,tenant,kind,state,not_before,created_at,updated_at) VALUES('legacy','t','work','pending',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "legacy"})
	if err != nil || len(js) != 1 || js[0].Priority != 0 {
		t.Fatalf("migrated default: %+v, %v", js, err)
	}
	if _, err := s.JobControl(ctx, store.JobControl{Tenant: "t", ID: "legacy", Action: store.JobSetPriority, Priority: 75}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	js, err = s.Jobs(ctx, store.JobQuery{Tenant: "t", ID: "legacy"})
	if err != nil || len(js) != 1 || js[0].Priority != 75 {
		t.Fatalf("reopened priority: %+v, %v", js, err)
	}
	if _, err := s.w.ExecContext(ctx, `UPDATE jobs SET priority = 101 WHERE id = 'legacy'`); err == nil {
		t.Fatal("migrated priority constraint missing")
	}
}
