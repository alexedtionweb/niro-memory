package api_test

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type saveOnlyPlanner struct {
	knowledge.Deterministic
	calls atomic.Int32
}

func (p *saveOnlyPlanner) Enrich(context.Context, knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	p.calls.Add(1)
	return knowledge.Enrichment{}, context.Canceled
}

func TestEnrichmentHTTPFastSaveAuthAndTenantIsolation(t *testing.T) {
	s := memstore.New(nil)
	defer s.Close()
	planner := &saveOnlyPlanner{}
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: planner, BackgroundEnrichment: true}})
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
	if planner.calls.Load() != 0 {
		t.Fatal("saving the original must not call the extraction model")
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

func TestExtractionProfileSettingsHTTPValidationAndIsolation(t *testing.T) {
	s := memstore.New(nil)
	defer s.Close()
	e, err := niromem.Open(niromem.Options{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	keys, err := api.NewKeys([]api.Key{{Key: adminKey, Tenant: "acme", Admin: true}, {Key: otherKey, Tenant: "other", Admin: true}, {Key: aliceKey, Tenant: "acme", User: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&api.Server{Engine: e, Auth: keys}).Handler())
	defer server.Close()
	p := knowledge.DefaultExtractionProfile()
	p.EntityTypes = []string{"person", "service"}
	p.EntityAttributes = map[string]knowledge.AttributeSchema{"role": {Type: "string"}}
	settings := niromem.Settings{Knowledge: &niromem.KnowledgePolicy{Extraction: &p}}
	if code, out := call(t, server, adminKey, "PUT", "/v1/settings", settings); code != 200 {
		t.Fatalf("profile write: %d %+v", code, out)
	}
	if code, out := call(t, server, adminKey, "GET", "/v1/settings", nil); code != 200 || out["knowledge"].(map[string]any)["extraction"] == nil {
		t.Fatalf("profile read: %d %+v", code, out)
	}
	if code, out := call(t, server, otherKey, "GET", "/v1/settings", nil); code != 200 || out["knowledge"] != nil {
		t.Fatalf("tenant profile leaked: %d %+v", code, out)
	}
	if code, _ := call(t, server, aliceKey, "PUT", "/v1/settings", settings); code != 403 {
		t.Fatalf("non-admin profile write: %d", code)
	}
	p.EntityAttributes["role"] = knowledge.AttributeSchema{Type: "object"}
	if code, _ := call(t, server, adminKey, "PUT", "/v1/settings", settings); code != 400 {
		t.Fatalf("unsafe profile accepted: %d", code)
	}
	if code, _ := call(t, server, adminKey, "PUT", "/v1/settings", map[string]any{"knowledge": map[string]any{"extraction": map[string]any{
		"entity_types": []string{"person"}, "fact_types": []string{"fact"}, "arbitrary_schema": true,
	}}}); code != 400 {
		t.Fatalf("unknown profile field silently ignored: %d", code)
	}
	if config, err := e.KnowledgeConfiguration(context.Background(), "acme"); err != nil || config.Effective.Extraction.EntityAttributes["role"].Type != "string" || len(config.Effective.ExtractionSchema) == 0 {
		t.Fatalf("failed profile corrupted durable settings: %+v %v", config, err)
	}
}
