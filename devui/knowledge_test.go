//go:build devui

package devui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/devui"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type observablePlanner struct{ calls atomic.Int32 }

func (p *observablePlanner) Plan(ctx context.Context, input knowledge.Input, state knowledge.State) (knowledge.Plan, error) {
	p.calls.Add(1)
	plan, err := (knowledge.Deterministic{}).Plan(ctx, input, state)
	plan.Provider, plan.Model = "test-provider", "test-model"
	if input.Text == "invalid proposal" {
		plan.Operations[0].Type = "NOT_AN_OPERATION"
	}
	return plan, err
}

func TestKnowledgeWorkspaceIntegration(t *testing.T) {
	ctx := context.Background()
	store := memstore.New(nil)
	provider := &observablePlanner{}
	opts := niromem.Options{Store: store, Knowledge: niromem.KnowledgeOptions{Mode: "disabled"},
		KnowledgePlanners: map[string]knowledge.Planner{"model": provider, "rules": knowledge.Deterministic{}}}
	engine, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	const admin = "workspace-admin-0123456789"
	const other = "workspace-other-0123456789"
	const user = "workspace-user-01234567890"
	keys, err := api.NewKeys([]api.Key{{Key: admin, Tenant: "a", Admin: true},
		{Key: other, Tenant: "b", Admin: true}, {Key: user, Tenant: "a", User: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/_dev/", devui.Handler(devui.Options{Engine: engine, Auth: keys}))
	mux.Handle("/", (&api.Server{Engine: engine, Auth: keys}).Handler())
	server := httptest.NewServer(mux)
	defer server.Close()
	do := func(method, path, key string, body any) (int, map[string]json.RawMessage) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Tenant", "forged")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	for _, tc := range []struct {
		key  string
		code int
	}{{"", 401}, {user, 403}, {admin, 200}} {
		code, _ := do("GET", "/_dev/api/knowledge/config", tc.key, nil)
		if code != tc.code {
			t.Fatalf("config authorization: %d != %d", code, tc.code)
		}
	}
	if code, out := do("POST", "/v1/knowledge/evaluate", admin, niromem.KnowledgeRequest{
		Input: knowledge.Input{Scope: memory.Scope{User: "u"}, OperationKey: "disabled", Text: "No preview"},
	}); code != 400 {
		t.Fatalf("disabled evaluation accepted: %d %s", code, out)
	}
	settings := niromem.Settings{DefaultPolicy: "deep", Knowledge: &niromem.KnowledgePolicy{
		Mode: "shadow", Planner: "model", MaxCandidates: 8, MaxGraphHops: 1}}
	code, out := do("PUT", "/v1/settings", admin, settings)
	if code != 200 {
		t.Fatalf("runtime shadow: %d %s", code, out)
	}
	code, out = do("GET", "/_dev/api/knowledge/config", admin, nil)
	var effective niromem.KnowledgeConfiguration
	json.Unmarshal(out["effective"], &effective)
	if code != 200 || effective.Mode != "shadow" || effective.MaxCandidates != 8 || string(out["runtime"]) != "true" {
		t.Fatalf("effective config: %d %s", code, out)
	}
	code, out = do("GET", "/_dev/api/knowledge/config", other, nil)
	json.Unmarshal(out["effective"], &effective)
	if code != 200 || effective.Mode != "disabled" {
		t.Fatalf("cross-tenant configuration: %s", out)
	}
	input := knowledge.Input{OperationKey: "dev-ui-add", Scope: memory.Scope{Tenant: "forged", User: "u"},
		Text: "Original assertion", Slot: "project.language", Intent: knowledge.ADD,
		Trust: memory.TrustHigh, Visibility: memory.Team}
	request := niromem.KnowledgeRequest{Input: input}
	for _, endpoint := range []string{"evaluate", "commit"} {
		code, out = do("POST", "/v1/knowledge/"+endpoint, admin, request)
		if code != 200 {
			t.Fatalf("%s: %d %s", endpoint, code, out)
		}
		if endpoint == "commit" && string(out["status"]) != `"shadow"` {
			t.Fatalf("shadow status: %s", out)
		}
	}
	ms, err := engine.List(ctx, niromem.ListRequest{Tenant: "a", User: "u"})
	if err != nil || len(ms) != 0 {
		t.Fatalf("shadow/evaluate wrote memories: %v %v", ms, err)
	}
	if code, _ := do("GET", "/v1/knowledge/traces/dev-ui-add", other, nil); code != 404 {
		t.Fatal("trace crossed tenant boundary")
	}
	settings.Knowledge.Mode = "enforced"
	settings.Knowledge.Planner = "rules"
	if code, out := do("PUT", "/v1/settings", user, settings); code != 403 {
		t.Fatalf("user can reconfigure: %d %s", code, out)
	}
	if code, out := do("PUT", "/v1/settings", admin, settings); code != 200 {
		t.Fatalf("enforce: %d %s", code, out)
	}
	// A second stateless engine sees durable runtime selection.
	reopened, err := niromem.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.KnowledgeConfiguration(ctx, "a")
	if err != nil || report.Effective.Mode != "enforced" || report.Effective.Provider != "deterministic" {
		t.Fatalf("durable config: %+v %v", report, err)
	}
	request.Input.OperationKey = "enforced-add"
	if code, out = do("POST", "/v1/knowledge/commit", admin, request); code != 200 || string(out["status"]) != `"committed"` {
		t.Fatalf("enforced commit: %d %s", code, out)
	}
	if code, out = do("POST", "/v1/knowledge/commit", admin, request); code != 200 || string(out["replayed"]) != "true" {
		t.Fatalf("commit replay: %d %s", code, out)
	}
	ms, err = engine.List(ctx, niromem.ListRequest{Tenant: "a", User: "u"})
	if err != nil || len(ms) != 1 {
		t.Fatalf("duplicate transition: %v %v", ms, err)
	}
	original := ms[0]
	before := time.Now().UTC()
	time.Sleep(time.Millisecond)
	request.Input.OperationKey, request.Input.Text = "enforced-correction", "Corrected assertion"
	request.Input.Intent, request.Input.TargetID = knowledge.CORRECT, original.ID
	request.Input.ValidFrom = original.ValidFrom
	if code, out = do("POST", "/v1/knowledge/commit", admin, request); code != 200 {
		t.Fatalf("correction: %d %s", code, out)
	}
	code, out = do("GET", "/v1/knowledge/traces/enforced-correction", admin, nil)
	if code != 200 || string(out["validation"]) != `"passed"` || out["proposed_plan"] == nil {
		t.Fatalf("missing actual validation/proposal: %d %s", code, out)
	}
	query := "/v1/knowledge/state?user_id=u&slot=project.language"
	for _, perspective := range []struct{ suffix, current string }{
		{"", "Corrected assertion"},
		{"&valid_at=" + before.Format(time.RFC3339Nano) + "&known_at=" + before.Format(time.RFC3339Nano), "Original assertion"},
		{"&valid_at=" + before.Format(time.RFC3339Nano), "Corrected assertion"},
	} {
		code, out = do("GET", query+perspective.suffix, admin, nil)
		if code != 200 || !strings.Contains(string(out["current"]), perspective.current) {
			t.Fatalf("temporal perspective: %d %s", code, out)
		}
	}
	if code, out = do("GET", "/v1/knowledge/relations/"+original.ID+"?user_id=u&all_versions=true", admin, nil); code != 200 || !strings.Contains(string(out["relations"]), "correct") {
		t.Fatalf("relationships: %d %s", code, out)
	}
	// Unknown profiles and oversized limits must be rejected even with no policies.
	for _, invalid := range []*niromem.KnowledgePolicy{
		{Mode: "invalid"}, {Planner: "missing"}, {MaxCandidates: 65}, {MaxGraphHops: 5},
	} {
		code, _ = do("PUT", "/v1/settings", admin, niromem.Settings{Knowledge: invalid})
		if code != 400 {
			t.Fatalf("invalid policy accepted: %+v", invalid)
		}
	}
	settings.Knowledge.Planner = "model"
	if code, out = do("PUT", "/v1/settings", admin, settings); code != 200 {
		t.Fatalf("provider switch: %d %s", code, out)
	}
	request.Input.OperationKey, request.Input.Text, request.Input.Intent = "bad-model", "invalid proposal", knowledge.ADD
	request.Input.TargetID, request.Input.Slot = "", ""
	if code, out = do("POST", "/v1/knowledge/commit", admin, request); code != 400 {
		t.Fatalf("invalid proposal: %d %s", code, out)
	}
	if code, out = do("GET", "/v1/knowledge/traces/bad-model", admin, nil); code != 200 ||
		string(out["error"]) != `"validation_failed"` || !strings.Contains(string(out["validation_error"]), "unknown_operation") {
		t.Fatalf("failed trace diagnostics: %d %s", code, out)
	}
	if provider.calls.Load() < 3 {
		t.Fatal("runtime model profile was never invoked")
	}
	for _, asset := range []string{"knowledge-ui.js", "pages/intelligence.js", "pages/transitions.js", "pages/knowledge-state.js"} {
		res, err := http.Get(server.URL + "/_dev/js/" + asset)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("missing asset %s", asset)
		}
	}
}

func TestKnowledgePolicyCannotElevateWithoutRuntimeProfiles(t *testing.T) {
	engine, err := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	err = engine.PutSettings(context.Background(), "tenant", niromem.Settings{
		Knowledge: &niromem.KnowledgePolicy{Mode: "enforced"},
	})
	if err == nil {
		t.Fatal("startup permissions were increased without runtime opt-in")
	}
}
