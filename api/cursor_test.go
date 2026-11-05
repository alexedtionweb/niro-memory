package api_test

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestExploreCursorSurvivesDeletionOfCursorRecord(t *testing.T) {
	ctx := context.Background()
	e, err := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, text := range []string{"one", "two", "three", "four", "five"} {
		if _, err := e.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: "dev"}, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer((&api.Server{Engine: e, Auth: api.Dev{}}).Handler())
	defer srv.Close()
	code, first := call(t, srv, "", "GET", "/v1/explore?limit=2", nil)
	if code != 200 || first["more"] != true {
		t.Fatalf("first %d %v", code, first)
	}
	cursor, ok := first["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatal("missing cursor")
	}
	if err := e.Delete(ctx, "dev", "test", cursor); err != nil {
		t.Fatal(err)
	}
	code, second := call(t, srv, "", "GET", "/v1/explore?limit=2&before_id="+url.QueryEscape(cursor), nil)
	if code != 200 {
		t.Fatalf("next %d %v", code, second)
	}
	rows := second["memories"].([]any)
	if len(rows) != 2 {
		t.Fatalf("wrong page size %v", second)
	}
	seen := map[string]bool{}
	for _, raw := range first["memories"].([]any) {
		seen[raw.(map[string]any)["id"].(string)] = true
	}
	for _, raw := range rows {
		id := raw.(map[string]any)["id"].(string)
		if seen[id] || id >= cursor {
			t.Fatalf("cursor repeated row %s", id)
		}
	}
	if code, out := call(t, srv, "", "GET", "/v1/memories?before_id="+url.QueryEscape(cursor)+"&offset=2", nil); code != 400 {
		t.Fatalf("mixed pagination accepted %d %v", code, out)
	}
}
