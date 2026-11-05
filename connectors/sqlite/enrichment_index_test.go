package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func TestEnrichmentIdentityLookupUsesIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f := memory.And(memory.Eq("metadata.enrichment_identity", "canonical"),
		memory.Filter{Field: "valid_to", Op: memory.OpExists, Value: false},
		memory.Filter{Field: "invalidated_at", Op: memory.OpExists, Value: false})
	if err := f.Check(); err != nil {
		t.Fatal(err)
	}
	where, args, err := s.where(store.Query{Tenant: "t", AllVersions: true, Filter: f})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.r.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT m.id FROM memories m WHERE "+where+" LIMIT 1", args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "memories_enrichment_identity")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("canonical identity lookup would scan tenant memories")
	}
}
