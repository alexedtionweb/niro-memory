package api_test

import (
	"context"
	"net/http/httptest"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type httpDocumentParser struct{}

type httpMediaParser struct{ httpDocumentParser }

func (httpMediaParser) Name() string { return "ollama/lfm3-omni:latest" }

func (httpDocumentParser) Validate(string, []byte) error { return nil }
func (httpDocumentParser) Extract(context.Context, string, []byte) ([]niromem.DocumentPage, error) {
	return []niromem.DocumentPage{{Number: 1, Text: "Atlas uses Redis."}}, nil
}

func TestDocumentHTTPUploadReplayAndAuthorization(t *testing.T) {
	engine, err := niromem.Open(niromem.Options{Store: memstore.New(nil), Blob: blob.NewMem(), Documents: httpDocumentParser{}, Media: httpMediaParser{}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	keys, err := api.NewKeys([]api.Key{{Key: aliceKey, Tenant: "acme", User: "alice", Agent: "assistant"},
		{Key: otherKey, Tenant: "other", Admin: true}, {Key: adminKey, Tenant: "acme", User: "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: engine, Auth: keys}).Handler())
	defer srv.Close()
	body := map[string]any{"name": "manual.txt", "bytes": []byte("Atlas uses Redis.")}
	code, out := call(t, srv, aliceKey, "POST", "/v1/documents", body)
	if code != 202 || out["status"] != "queued" {
		t.Fatalf("upload: %d %v", code, out)
	}
	id := out["id"].(string)
	for _, key := range []string{otherKey, adminKey} {
		if code, _ := call(t, srv, key, "GET", "/v1/documents/"+id+"/preview", nil); code != 404 {
			t.Fatalf("preview leaked original: %d", code)
		}
	}
	if code, replay := call(t, srv, aliceKey, "POST", "/v1/documents", body); code != 202 || replay["id"] != id {
		t.Fatalf("replay: %d %v", code, replay)
	}
	for _, key := range []string{otherKey, adminKey} {
		if code, out := call(t, srv, key, "GET", "/v1/documents/"+id, nil); code != 404 {
			t.Fatalf("status leaked: %d %v", code, out)
		}
		if code, out := call(t, srv, key, "GET", "/v1/documents", nil); code != 200 || len(out["documents"].([]any)) != 0 {
			t.Fatalf("list leaked: %d %v", code, out)
		}
	}
	if code, out := call(t, srv, aliceKey, "GET", "/v1/documents", nil); code != 200 || len(out["documents"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
	if code, _ := call(t, srv, "", "POST", "/v1/documents", body); code != 401 {
		t.Fatalf("anonymous upload: %d", code)
	}
	body["user_id"] = "bob"
	if code, _ := call(t, srv, aliceKey, "POST", "/v1/documents", body); code != 403 {
		t.Fatalf("forged scope: %d", code)
	}
	delete(body, "user_id")
	body["name"] = "photo.png"
	code, media := call(t, srv, aliceKey, "POST", "/v1/media", body)
	if code != 202 || media["kind"] != "media" || media["unreviewed"] != true {
		t.Fatalf("media upload: %d %v", code, media)
	}
	if code, out := call(t, srv, otherKey, "GET", "/v1/documents/"+media["id"].(string), nil); code != 404 {
		t.Fatalf("media tenant leaked: %d %v", code, out)
	}
}
