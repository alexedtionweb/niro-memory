package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/metrics"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

const (
	adminKey = "admin-key-0123456789"
	aliceKey = "alice-key-0123456789"
	otherKey = "other-tenant-key-0123"
)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	b, _ := os.ReadFile("../configs/rules.example.json")
	rs, err := rules.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := niromem.Open(niromem.Options{Store: memstore.New(nil), Rules: rs, Blob: blob.NewMem(), Debounce: -1})
	keys, err := api.NewKeys([]api.Key{
		{Key: adminKey, Tenant: "acme", Admin: true},
		{Key: aliceKey, Tenant: "acme", User: "alice", Agent: "assistant"},
		{Key: otherKey, Tenant: "globex", Admin: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: e, Auth: keys, Metrics: metrics.New()}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, key, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if key != "" {
		req.Header.Set("Authorization", "Token "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		var arr []any
		if json.Unmarshal(raw, &arr) == nil {
			out = map[string]any{"_list": arr}
		}
	}
	return resp.StatusCode, out
}

func TestAuthAndBinding(t *testing.T) {
	srv := server(t)
	if code, _ := call(t, srv, "", "GET", "/v1/capabilities", nil); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := call(t, srv, "wrong-key-0000000000", "GET", "/v1/capabilities", nil); code != 401 {
		t.Fatal("bad key accepted")
	}
	code, out := call(t, srv, aliceKey, "POST", "/v1/facts", map[string]any{"facts": []map[string]any{{"text": "Alice likes jazz", "user_id": "bob"}}})
	if code != 403 {
		t.Fatalf("bound key acting as another user: %d %v", code, out)
	}
	code, out = call(t, srv, aliceKey, "POST", "/v1/facts", map[string]any{"facts": []map[string]any{{"text": "Alice likes jazz"}}})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	mem := out["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)
	if mem["user_id"] != "alice" || mem["agent_id"] != "assistant" || mem["tenant"] != "acme" {
		t.Fatalf("scope not forced from key: %v", mem)
	}
	// Another tenant cannot see it, even with an admin key.
	if code, _ := call(t, srv, otherKey, "GET", "/v1/memories/"+mem["id"].(string), nil); code != 404 {
		t.Fatalf("cross-tenant read: %d", code)
	}
	if code, _ := call(t, srv, aliceKey, "PUT", "/v1/settings", map[string]any{}); code != 403 {
		t.Fatal("non-admin changed settings")
	}
	if code, _ := call(t, srv, adminKey, "POST", "/v1/search", map[string]any{"query": "jazz", "filter": map[string]any{"x": map[string]any{"regex": "."}}}); code != 400 {
		t.Fatal("bad filter must be 400")
	}
	if code, out := call(t, srv, "", "GET", "/metrics", nil); code != 200 && out == nil {
		t.Fatal("metrics endpoint")
	}
}

func TestClientFlows(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := client.New(srv.URL, aliceKey)
	ch, err := c.Remember(ctx, niromem.Fact{Text: "Alice's favourite city is Kyoto", Slot: "user.favourite_city"})
	if err != nil || len(ch) != 1 {
		t.Fatal(err)
	}
	_, err = c.Append(ctx, "chat-1", "", "", []niromem.Turn{{Role: "user", Text: "I live in Lisbon."}, {Role: "assistant", Text: "Great city!"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Search(ctx, client.SearchRequest{SearchRequest: niromem.SearchRequest{Query: "where do I live", Explain: true}})
	if err != nil || len(res.Hits) == 0 || res.Hits[0].Memory.Text != "User lives in Lisbon" {
		t.Fatalf("search: %+v %v", res, err)
	}
	_, rendered, err := c.Context(ctx, niromem.ContextRequest{Query: "kyoto", Timeout: time.Second})
	if err != nil || !strings.Contains(rendered, "Kyoto") || !strings.HasPrefix(rendered, "<memory-context>") {
		t.Fatalf("context: %q %v", rendered, err)
	}
	w, err := c.Window(ctx, "chat-1", 100)
	if err != nil || len(w.Turns) != 2 {
		t.Fatalf("window: %+v %v", w, err)
	}
	upd, err := c.Update(ctx, ch[0].Memory.ID, niromem.Patch{Text: ptr("Alice's favourite city is Kyoto, Japan")})
	if err != nil || upd.Version != 2 {
		t.Fatalf("update: %+v %v", upd, err)
	}
	h, _ := c.History(ctx, ch[0].Memory.ID)
	if len(h) != 2 {
		t.Fatalf("history: %+v", h)
	}
	if _, err := c.Get(ctx, "missing"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("not found mapping: %v", err)
	}
	// Alice cannot read another run's turns.
	admin := client.New(srv.URL, adminKey)
	if _, err := admin.Append(ctx, "bob-chat", "bob", "", []niromem.Turn{{Role: "user", Text: "secret"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Window(ctx, "bob-chat", 100); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("read another user's run: %v", err)
	}
	if fr, err := c.Forget(ctx, memory.Scope{}); err != nil || fr.Memories == 0 {
		t.Fatalf("forget: %+v %v", fr, err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestMem0Compat(t *testing.T) {
	srv := server(t)
	// Mem0 platform client: add with inference (rules), then search.
	code, out := call(t, srv, adminKey, "POST", "/mem0/v3/memories/add/", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "Hi, my name is Maria. I work at Globex."}}, "user_id": "maria"})
	if code != 200 || len(out["results"].([]any)) != 2 || out["results"].([]any)[0].(map[string]any)["event"] != "ADD" {
		t.Fatalf("add: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "POST", "/mem0/v3/memories/search/", map[string]any{"query": "which company employs her? work", "filters": map[string]any{"user_id": "maria"}, "top_k": 1})
	res := out["results"].([]any)
	if code != 200 || len(res) != 1 || res[0].(map[string]any)["memory"] != "User works at Globex" || res[0].(map[string]any)["score"] == nil {
		t.Fatalf("search: %d %v", code, out)
	}
	id := res[0].(map[string]any)["id"].(string)
	// OSS server shapes.
	code, out = call(t, srv, adminKey, "POST", "/mem0/memories", map[string]any{"messages": "I prefer tea", "user_id": "maria", "infer": false})
	if code != 200 || out["results"].([]any)[0].(map[string]any)["memory"] != "I prefer tea" {
		t.Fatalf("infer=false: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "GET", "/mem0/memories?user_id=maria", nil)
	if code != 200 || len(out["results"].([]any)) != 3 {
		t.Fatalf("get_all: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "POST", "/mem0/v3/memories/", map[string]any{"filters": map[string]any{"AND": []any{map[string]any{"user_id": "maria"}, map[string]any{"text": map[string]any{"icontains": "globex"}}}}})
	if code != 200 || out["count"].(float64) != 1 {
		t.Fatalf("v3 get_all filters: %d %v", code, out)
	}
	if code, out = call(t, srv, adminKey, "PUT", "/mem0/v1/memories/"+id+"/", map[string]any{"text": "User works at Globex Corp"}); code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "GET", "/mem0/v1/memories/"+id+"/history/", nil)
	if code != 200 || len(out["_list"].([]any)) != 2 || out["_list"].([]any)[1].(map[string]any)["event"] != "UPDATE" {
		t.Fatalf("history: %d %v", code, out)
	}
	if code, _ = call(t, srv, adminKey, "DELETE", "/mem0/v1/memories/?user_id=maria", nil); code != 200 {
		t.Fatal("delete all")
	}
	if code, out = call(t, srv, adminKey, "GET", "/mem0/memories?user_id=maria", nil); len(out["results"].([]any)) != 0 {
		t.Fatalf("after delete all: %v", out)
	}
}

func TestErrorsAndLimits(t *testing.T) {
	srv := server(t)
	if code, _ := call(t, srv, adminKey, "POST", "/v1/runs/r/turns", map[string]any{"turns": []map[string]any{{"role": "robot", "text": "x"}}}); code != 400 {
		t.Fatal("bad role must be 400")
	}
	big := strings.Repeat("a", 40<<20)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/facts", strings.NewReader(`{"facts":[{"text":"`+big+`"}]}`))
	req.Header.Set("X-API-Key", adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 413 {
		t.Fatalf("oversized body: %v %v", resp.StatusCode, err)
	}
	if code, _ := call(t, srv, adminKey, "PATCH", "/v1/memories/nope", map[string]any{"text": "x"}); code != 404 {
		t.Fatal("patch missing must be 404")
	}
}
