package blob_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/memory"
)

func TestStores(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]blob.Store{"mem": blob.NewMem(), "dir": blob.Dir{Root: t.TempDir()}} {
		t.Run(name, func(t *testing.T) {
			ref, err := s.Put(ctx, "t1", []byte("hello"), "text/plain")
			if err != nil || ref.Size != 5 || len(ref.SHA256) != 64 {
				t.Fatalf("%+v %v", ref, err)
			}
			again, _ := s.Put(ctx, "t1", []byte("hello"), "text/plain")
			if again.SHA256 != ref.SHA256 {
				t.Fatal("content addressing")
			}
			if b, err := s.Get(ctx, "t1", ref.SHA256); err != nil || string(b) != "hello" {
				t.Fatalf("%s %v", b, err)
			}
			if _, err := s.Get(ctx, "t2", ref.SHA256); !errors.Is(err, memory.ErrNotFound) {
				t.Fatal("blob readable from another tenant")
			}
			if err := s.Delete(ctx, "t1", ref.SHA256); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(ctx, "t1", ref.SHA256); !errors.Is(err, memory.ErrNotFound) {
				t.Fatal("not deleted")
			}
		})
	}
	d := blob.Dir{Root: t.TempDir()}
	if _, err := d.Get(ctx, "../etc", "00"); !errors.Is(err, memory.ErrInvalid) {
		t.Fatal("path traversal not rejected")
	}
}
