package api_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestKnowledgeHTTPIdentityAndDryRun(t *testing.T) {
	engine, err := niromem.Open(niromem.Options{
		Store: memstore.New(nil),
		Knowledge: niromem.KnowledgeOptions{
			Mode: "enforced", Planner: knowledge.Deterministic{}, Timeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	if _, err := engine.Remember(context.Background(), niromem.Fact{
		Scope: memory.Scope{Tenant: "acme", User: "alice", Agent: "assistant"},
		Text:  "The service uses Go", Slot: "project.language",
	}); err != nil {
		t.Fatal(err)
	}
	records, err := engine.List(context.Background(), niromem.ListRequest{Tenant: "acme", User: "alice", Agent: "assistant"})
	if err != nil || len(records) != 1 {
		t.Fatalf("seed record: %d %v", len(records), err)
	}
	keys, err := api.NewKeys([]api.Key{{Key: aliceKey, Tenant: "acme", User: "alice", Agent: "assistant"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: engine, Auth: keys}).Handler())
	defer srv.Close()
	if code, out := call(t, srv, aliceKey, "GET", "/v1/knowledge/state?slot=project.language", nil); code != 200 {
		t.Fatalf("state: %d %v", code, out)
	}
	if code, out := call(t, srv, aliceKey, "GET", "/v1/knowledge/relations/"+records[0].ID+"?depth=1", nil); code != 200 {
		t.Fatalf("relations: %d %v", code, out)
	}

	request := niromem.KnowledgeRequest{Input: knowledge.Input{
		OperationKey: "http-correction", Scope: memory.Scope{Tenant: "attacker", User: "alice", Agent: "assistant"},
		Text: "The service uses Rust", Slot: "project.language", TargetID: records[0].ID,
		Intent: knowledge.CORRECT, Trust: memory.TrustHigh, Authority: true,
		Assertion: &memory.Assertion{Subject: "service", Predicate: "language", Object: "Rust", EpistemicType: memory.EpistemicFact, Confidence: 1},
	}}
	code, out := call(t, srv, aliceKey, "POST", "/v1/knowledge/evaluate", request)
	if code != 200 {
		t.Fatalf("evaluate: %d %v", code, out)
	}
	if out["scope"].(map[string]any)["tenant"] != "acme" {
		t.Fatalf("request tenant was not replaced: %v", out["scope"])
	}
	op := out["plan"].(map[string]any)["operations"].([]any)[0].(map[string]any)
	if op["type"] != string(knowledge.DEFER) || op["reason_code"] != "authority_required" {
		t.Fatalf("client-supplied authority was trusted: %v", op)
	}
	after, err := engine.List(context.Background(), niromem.ListRequest{Tenant: "acme", User: "alice", Agent: "assistant"})
	if err != nil || len(after) != 1 {
		t.Fatalf("evaluate mutated knowledge: %d %v", len(after), err)
	}

	request.Input.OperationKey = ""
	if code, _ := call(t, srv, aliceKey, "POST", "/v1/knowledge/commit", request); code != 400 {
		t.Fatalf("commit without operation key: %d", code)
	}
	lowTrust := niromem.KnowledgeRequest{Input: knowledge.Input{
		OperationKey: "low-trust-add", Scope: memory.Scope{Tenant: "attacker", User: "alice", Agent: "assistant"},
		Text: "An external source claims the project uses Rust", Slot: "project.external_language",
		Intent: knowledge.ADD, Trust: memory.TrustLow,
	}}
	code, _ = call(t, srv, aliceKey, "POST", "/v1/knowledge/commit", lowTrust)
	if code != 200 {
		t.Fatalf("low-trust commit: %d", code)
	}
	after, err = engine.List(context.Background(), niromem.ListRequest{Tenant: "acme", User: "alice", Agent: "assistant"})
	if err != nil || len(after) != 2 {
		t.Fatalf("low-trust add: %d %v", len(after), err)
	}
	if code, out := call(t, srv, aliceKey, "GET", "/v1/knowledge/traces/low-trust-add", nil); code != 200 {
		t.Fatalf("trace: %d %v", code, out)
	}
	for _, m := range after {
		if m.Slot == "project.external_language" && m.Source.Trust != memory.TrustLow {
			t.Fatalf("explicit low trust was promoted: %q", m.Source.Trust)
		}
	}
}
