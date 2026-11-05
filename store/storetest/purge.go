package storetest

import (
	"errors"
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func testTenantPurge(t *testing.T, s store.Store) {
	p, ok := s.(store.TenantPurger)
	if !ok {
		t.Skip("no tenant purge capability")
	}
	for _, tenant := range []string{"wipe", "keep"} {
		b := &store.Batch{Tenant: tenant, Settings: []byte(`{"custom":true}`), OperationKey: "saved", OperationHash: "h"}
		for i := 0; i < 5; i++ {
			m := mem(memory.NewID(), "source")
			m.Tenant = tenant
			b.Memories = append(b.Memories, m)
		}
		b.Episodes = []memory.Episode{{Scope: memory.Scope{Run: "run"}, Role: "user", Parts: []memory.Part{{Kind: "text", Text: "evidence"}}}}
		b.Jobs = []store.Job{{Kind: "ordinary", Key: "key"}}
		if _, err := s.Write(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	status, err := p.BeginPurge(ctx, "wipe", "request")
	if err != nil || status.State != "purging" {
		t.Fatalf("begin %+v %v", status, err)
	}
	if again, err := p.BeginPurge(ctx, "wipe", "request"); err != nil || again.ID != status.ID {
		t.Fatalf("replay %+v %v", again, err)
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "wipe", Settings: []byte(`{}`)}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("writes not fenced: %v", err)
	}
	var prior int64
	done := false
	for i := 0; i < 100 && !done; i++ {
		status, done, err = p.PurgeBatch(ctx, "wipe", 2)
		if err != nil {
			t.Fatal(err)
		}
		if status.Deleted < prior {
			t.Fatal("progress regressed")
		}
		prior = status.Deleted
	}
	if !done {
		t.Fatal("purge did not finish")
	}
	if err := p.FinishPurge(ctx, "wipe"); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"wipe", "keep"} {
		ms, err := s.Find(ctx, store.Query{Tenant: tenant, AllVersions: true, ShowExpired: true, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		eps, err := s.Episodes(ctx, store.EpisodeQuery{Tenant: tenant, IncludeDeleted: true, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if tenant == "wipe" && (len(ms) != 0 || len(eps) != 0) {
			t.Fatalf("data retained: %d %d", len(ms), len(eps))
		}
		if tenant == "keep" && (len(ms) != 5 || len(eps) != 1) {
			t.Fatalf("other tenant damaged: %d %d", len(ms), len(eps))
		}
	}
	raw, err := s.Settings(ctx, "wipe")
	if err != nil || len(raw) != 0 {
		t.Fatalf("settings retained %s %v", raw, err)
	}
	if ops, ok := s.(store.Operations); ok {
		if _, err := ops.Operation(ctx, "wipe", "saved"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("receipt retained %v", err)
		}
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "wipe", Settings: []byte(`{}`)}); err != nil {
		t.Fatalf("empty tenant not unlocked: %v", err)
	}
	if replay, err := p.BeginPurge(ctx, "wipe", "request"); err != nil || replay.State != "complete" {
		t.Fatalf("completed replay: %+v %v", replay, err)
	}
	if _, err := p.BeginPurge(ctx, "wipe", "second"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		_, done, err := p.PurgeBatch(ctx, "wipe", 2)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if err := p.FinishPurge(ctx, "wipe"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, &store.Batch{Tenant: "wipe", Settings: []byte(`{"new":true}`)}); err != nil {
		t.Fatal(err)
	}
	if replay, err := p.BeginPurge(ctx, "wipe", "request"); err != nil || replay.State != "complete" {
		t.Fatalf("old replay started another wipe %+v %v", replay, err)
	}
	if raw, err := s.Settings(ctx, "wipe"); err != nil || string(raw) != `{"new":true}` {
		t.Fatalf("old replay damaged new data %s %v", raw, err)
	}
}
