package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/connectors/postgres"
	"github.com/alexedtionweb/niro-memory/enginetest"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/storetest"
	"github.com/alexedtionweb/niro-memory/vector"
	"github.com/alexedtionweb/niro-memory/vector/vectortest"
)

// Set NIROMEM_PG_DSN (e.g. from deploy/docker-compose.yml) to run:
//
//	NIROMEM_PG_DSN=postgres://niromem:niromem@localhost:5432/niromem?sslmode=disable go test ./...
var seq atomic.Int64

func open(t *testing.T) *postgres.Store {
	dsn := os.Getenv("NIROMEM_PG_DSN")
	if dsn == "" {
		t.Skip("NIROMEM_PG_DSN not set")
	}
	schema := fmt.Sprintf("niromem_test_%d_%d", time.Now().UnixNano()%1e9, seq.Add(1))
	s, err := postgres.Open(context.Background(), postgres.Options{DSN: dsn, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}

// TestConformanceWithHNSW reruns the suite with an HNSW index for the
// suite's 3-dimensional model "m", so Search goes through pgvector.
func TestConformanceWithHNSW(t *testing.T) {
	probe := open(t)
	ok := probe.HasPGVector()
	probe.Close()
	if !ok {
		t.Skip("pgvector not installed")
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		s := open(t)
		if err := s.EnsureVectorIndex(context.Background(), "m", 3); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// TestConformanceRLS runs the suite with row-level security enabled. Run it
// as a non-owner role without BYPASSRLS to have the policies enforced.
func TestConformanceRLS(t *testing.T) {
	dsn := os.Getenv("NIROMEM_PG_DSN")
	if dsn == "" {
		t.Skip("NIROMEM_PG_DSN not set")
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		schema := fmt.Sprintf("niromem_rls_%d_%d", time.Now().UnixNano()%1e9, seq.Add(1))
		s, err := postgres.Open(context.Background(), postgres.Options{DSN: dsn, Schema: schema, RLS: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestEngine(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}

func TestVectorIndex(t *testing.T) {
	vectortest.Run(t, func(t *testing.T) vector.Index { return open(t) })
}
