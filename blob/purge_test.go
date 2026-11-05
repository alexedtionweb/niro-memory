package blob

import (
	"context"
	"errors"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
)

func TestTenantPurgeIsBoundedAndIsolated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store interface {
			Store
			TenantPurger
		}
	}{
		{"memory", NewMem()}, {"directory", Dir{Root: t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var refs []memory.BlobRef
			for i := 0; i < 7; i++ {
				ref, err := tc.store.Put(ctx, "wipe", []byte{byte(i)}, "application/octet-stream")
				if err != nil {
					t.Fatal(err)
				}
				refs = append(refs, ref)
			}
			foreign, err := tc.store.Put(ctx, "keep", []byte("keep"), "text/plain")
			if err != nil {
				t.Fatal(err)
			}
			total := 0
			done := false
			for i := 0; i < 10 && !done; i++ {
				n, finished, err := tc.store.PurgeTenantBatch(ctx, "wipe", 2)
				if err != nil {
					t.Fatal(err)
				}
				if n > 2 {
					t.Fatalf("unbounded batch %d", n)
				}
				total += n
				done = finished
			}
			if !done || total != 7 {
				t.Fatalf("incomplete %t %d", done, total)
			}
			for _, ref := range refs {
				if _, err := tc.store.Get(ctx, "wipe", ref.SHA256); !errors.Is(err, memory.ErrNotFound) {
					t.Fatalf("retained blob %v", err)
				}
			}
			if _, err := tc.store.Get(ctx, "keep", foreign.SHA256); err != nil {
				t.Fatalf("other tenant removed %v", err)
			}
			if _, _, err := tc.store.PurgeTenantBatch(ctx, "../keep", 2); err == nil {
				t.Fatal("unsafe tenant accepted")
			}
		})
	}
}
