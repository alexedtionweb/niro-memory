package niro_test

import (
	"context"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/connectors/niro"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	nmem "github.com/alexedtionweb/niro-stream/memory"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	e, _ := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	var s nmem.Store = &niro.Store{Engine: e, Tenant: "t", User: "u"}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first, err := s.Put(ctx, nmem.Record{Scope: "room", Source: "user", Body: "one", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, nmem.Record{ID: first.ID, Scope: "room", Source: "user", Body: "replaced", At: at}); err != nil {
		t.Fatal(err)
	}
	s.Put(ctx, nmem.Record{Scope: "room", Source: "capture", Body: "door", Ref: "frame:1", Tags: []string{"live", "camera"}, Meta: map[string]string{"cam": "a"}, At: at.Add(time.Second)})
	s.Put(ctx, nmem.Record{Scope: "room", Source: "tool", Body: "three", At: at.Add(2 * time.Second)})
	s.Put(ctx, nmem.Record{Scope: "other", Source: "tool", Body: "elsewhere"})
	if _, err := s.Put(ctx, nmem.Record{}); err == nil {
		t.Fatal("empty record accepted")
	}
	got, err := s.Recall(ctx, nmem.Query{Scope: "room", Limit: 2})
	if err != nil || len(got) != 2 || got[0].Body != "door" || got[1].Body != "three" {
		t.Fatalf("%+v %v", got, err)
	}
	all, _ := s.Recall(ctx, nmem.Query{Scope: "room"})
	if len(all) != 3 || all[0].Body != "replaced" || all[0].ID != first.ID {
		t.Fatalf("replace by id: %+v", all)
	}
	cam, _ := s.Recall(ctx, nmem.Query{Scope: "room", Sources: []string{"capture"}, Tags: []string{"live"}})
	if len(cam) != 1 || cam[0].Ref != "frame:1" || cam[0].Meta["cam"] != "a" || cam[0].Source != "capture" {
		t.Fatalf("%+v", cam)
	}
	if n, err := s.Forget(ctx, nmem.Query{Scope: "room", Sources: []string{"tool"}}); err != nil || n != 1 {
		t.Fatalf("forget: %d %v", n, err)
	}
	if left, _ := s.Recall(ctx, nmem.Query{Scope: "room"}); len(left) != 2 {
		t.Fatalf("after forget: %+v", left)
	}
	if any, _ := s.Recall(ctx, nmem.Query{AnyScope: true}); len(any) != 3 {
		t.Fatalf("any scope: %+v", any)
	}
}
