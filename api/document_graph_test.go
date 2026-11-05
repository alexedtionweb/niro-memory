package api_test

import (
	"context"
	"net/http/httptest"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestDocumentGraphAndRevisionHTTP(t *testing.T) {
	s := memstore.New(nil)
	engine, err := niromem.Open(niromem.Options{Store: s, Blob: blob.NewMem(), Documents: httpDocumentParser{}, Embedder: embed.Hash{Dims: 8}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	keys, err := api.NewKeys([]api.Key{{Key: aliceKey, Tenant: "acme", User: "alice"}, {Key: adminKey, Tenant: "acme", Admin: true}, {Key: otherKey, Tenant: "other", Admin: true}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: engine, Auth: keys}).Handler())
	defer srv.Close()
	code, out := call(t, srv, aliceKey, "POST", "/v1/documents", map[string]any{"name": "manual.txt", "bytes": []byte("Atlas uses Redis.")})
	if code != 202 {
		t.Fatalf("import: %d %v", code, out)
	}
	id := out["id"].(string)
	js, err := s.Jobs(context.Background(), store.JobQuery{Tenant: "acme", Kind: niromem.JobDocumentImport})
	if err != nil || len(js) != 1 {
		t.Fatalf("jobs: %v %v", js, err)
	}
	if err := engine.Worker().Handlers[niromem.JobDocumentImport](context.Background(), js[0]); err != nil {
		t.Fatal(err)
	}
	path := "/v1/documents/" + id
	if code, out = call(t, srv, aliceKey, "GET", path+"/graph", nil); code != 200 || out["total_chunks"] != float64(1) || out["is_current"] != true {
		t.Fatalf("graph: %d %v", code, out)
	}
	nodes := out["nodes"].([]any)
	chunk := nodes[1].(map[string]any)["memory_id"].(string)
	if len(nodes) != 2 {
		t.Fatal("graph fabricated missing embeddings")
	}
	for _, suffix := range []string{"/graph", "/revisions", "/revisions?revision_id=original"} {
		if code, _ := call(t, srv, otherKey, "GET", path+suffix, nil); code != 404 {
			t.Fatalf("foreign tenant read %s: %d", suffix, code)
		}
	}
	if code, out = call(t, srv, aliceKey, "GET", "/v1/memories/"+chunk+"/document", nil); code != 200 || out["document_id"] != id {
		t.Fatalf("lineage: %d %v", code, out)
	}
	for _, query := range []string{"?limit=51", "?after_id=foreign-memory", "?revision_id=foreign-revision"} {
		if code, _ := call(t, srv, aliceKey, "GET", path+"/graph"+query, nil); code == 200 {
			t.Fatalf("invalid graph input accepted: %s", query)
		}
	}
	request := niromem.DocumentReprocessRequest{RequestID: "native-revision", Stages: []string{"embeddings"}}
	if code, _ := call(t, srv, aliceKey, "POST", path+"/reprocess", request); code != 403 {
		t.Fatalf("bound key queued admin processing: %d", code)
	}
	if code, _ := call(t, srv, otherKey, "POST", path+"/reprocess", request); code != 404 {
		t.Fatalf("foreign tenant queued processing: %d", code)
	}
	code, out = call(t, srv, adminKey, "POST", path+"/reprocess", request)
	if code != 202 || out["state"] != "queued" {
		t.Fatalf("queue: %d %v", code, out)
	}
	revision := out["id"].(string)
	js, err = s.Jobs(context.Background(), store.JobQuery{Tenant: "acme", Kind: niromem.JobDocumentReprocess})
	if err != nil || len(js) != 1 {
		t.Fatalf("revision job: %v %v", js, err)
	}
	if err := engine.Worker().Handlers[niromem.JobDocumentReprocess](context.Background(), js[0]); err != nil {
		t.Fatal(err)
	}
	if code, out = call(t, srv, aliceKey, "GET", path+"/revisions?revision_id="+revision, nil); code != 200 || out["state"] != "committed" {
		t.Fatalf("revision: %d %v", code, out)
	}
	if out["progress"].(map[string]any)["stage"] != "embeddings" {
		t.Fatalf("missing persisted progress: %v", out)
	}
	if code, out = call(t, srv, aliceKey, "GET", path+"/graph", nil); code != 200 || len(out["nodes"].([]any)) != 3 {
		t.Fatalf("persisted embedding missing: %d %v", code, out)
	}
	if code, out = call(t, srv, adminKey, "POST", path+"/reprocess", request); code != 202 || out["id"] != revision || out["state"] != "committed" {
		t.Fatalf("replay: %d %v", code, out)
	}
}
