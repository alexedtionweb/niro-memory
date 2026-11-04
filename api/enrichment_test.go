package api_test

import (
	"context"
	"net/http/httptest"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestEnrichmentHTTPFastSaveAuthAndTenantIsolation(t *testing.T) {
	s := memstore.New(nil)
	defer s.Close()
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: knowledge.Deterministic{}, BackgroundEnrichment: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	const other = "other-tenant-key-012345678901"
	keys, err := api.NewKeys([]api.Key{{Key: aliceKey, Tenant: "acme", User: "alice"}, {Key: other, Tenant: "other", User: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&api.Server{Engine: e, Auth: keys}).Handler())
	defer server.Close()
	body := map[string]any{"facts": []niromem.Fact{{Scope: memory.Scope{Tenant: "forged"}, Text: "Atlas uses Redis.", OperationKey: "retry", Visibility: memory.Private}}}
	code, out := call(t, server, aliceKey, "POST", "/v1/facts", body)
	if code != 200 {
		t.Fatalf("save: %d %+v", code, out)
	}
	id := out["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)["id"].(string)
	path := "/v1/knowledge/enrichment/" + id
	if code, out := call(t, server, aliceKey, "GET", path, nil); code != 200 || out["status"] != "queued" {
		t.Fatalf("status: %d %+v", code, out)
	}
	if code, _ := call(t, server, other, "GET", path, nil); code != 404 {
		t.Fatalf("cross-tenant status returned %d", code)
	}
	if code, _ := call(t, server, "", "GET", path, nil); code != 401 {
		t.Fatalf("unauthenticated status returned %d", code)
	}
	if _, err := e.EnrichmentStatus(context.Background(), "forged", memory.Principal{Admin: true}, id); err == nil {
		t.Fatal("forged tenant persisted")
	}
}
