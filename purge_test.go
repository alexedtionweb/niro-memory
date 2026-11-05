package niromem_test

import (
	"context"
	"errors"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestTenantCleanupRemovesOriginalsAndFencesWrites(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	b := blob.Dir{Root: t.TempDir()}
	e, err := niromem.Open(niromem.Options{Store: s, Blob: b})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ref, err := b.Put(ctx, "wipe", []byte("original"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.Put(ctx, "keep", []byte("original"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "wipe", User: "u"}, Text: "some memory"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.PurgeTenant(ctx, "wipe", "request"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "wipe", User: "u"}, Text: "must not recreate"}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("write fence %v", err)
	}
	if _, err = e.Worker().RunOnce(ctx, 1); err != nil {
		t.Fatal(err)
	}
	status, err := e.TenantPurgeStatus(ctx, "wipe")
	if err != nil || status.State != "complete" {
		t.Fatalf("status %+v %v", status, err)
	}
	if _, err = b.Get(ctx, "wipe", ref.SHA256); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("original remains %v", err)
	}
	if _, err = b.Get(ctx, "keep", other.SHA256); err != nil {
		t.Fatalf("foreign original removed %v", err)
	}
	if jobs, err := s.Jobs(ctx, store.JobQuery{Tenant: "wipe"}); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs remain %+v %v", jobs, err)
	}
}
