package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type workspaceParser struct{}

func (workspaceParser) Validate(string, []byte) error { return nil }
func (workspaceParser) Extract(_ context.Context, _ string, data []byte) ([]niromem.DocumentPage, error) {
	return []niromem.DocumentPage{{Number: 1, Text: string(data), Model: "fixture/omni"}}, nil
}
func (workspaceParser) Name() string { return "fixture/omni" }

type workspaceFixture struct {
	engine              *niromem.Engine
	store               *memstore.Store
	admin, bound, other *Server
}

func newWorkspaceFixture(t *testing.T) workspaceFixture {
	t.Helper()
	s := memstore.New(nil)
	e, err := niromem.Open(niromem.Options{
		Store: s, Blob: blob.NewMem(), Documents: workspaceParser{}, Media: workspaceParser{},
		Embedder:  embed.Hash{Dims: 8},
		Knowledge: niromem.KnowledgeOptions{Mode: "enforced", Planner: knowledge.Deterministic{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	auth, err := api.NewKeys([]api.Key{
		{Key: "admin-workspace-test-key", Tenant: "one", Admin: true},
		{Key: "bound-workspace-test-key", Tenant: "one", User: "project"},
		{Key: "other-workspace-test-key", Tenant: "two", Admin: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := (&api.Server{Engine: e, Auth: auth}).Handler()
	makeServer := func(key string) *Server {
		c := client.NewLocal(handler)
		c.Key = key
		server, err := NewWorkspace(c, "project", "")
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	return workspaceFixture{e, s, makeServer("admin-workspace-test-key"), makeServer("bound-workspace-test-key"), makeServer("other-workspace-test-key")}
}

func workspaceCall(t *testing.T, s *Server, name string, args any, wantError bool) json.RawMessage {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr := s.call(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("%s RPC error: %+v", name, rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil || len(response.Content) != 1 || response.IsError != wantError {
		t.Fatalf("%s result: %s (%v)", name, encoded, err)
	}
	return json.RawMessage(response.Content[0].Text)
}

func TestWorkspaceReadsMatchUIIdentityNotHiddenProjectFilter(t *testing.T) {
	f := newWorkspaceFixture(t)
	for _, scope := range []memory.Scope{
		{Tenant: "one", User: "project"}, {Tenant: "one", User: "another-project"}, {Tenant: "two", User: "project"},
	} {
		_, err := f.engine.Remember(context.Background(), niromem.Fact{
			Scope: scope, Text: scope.Tenant + "/" + scope.User + " private convention", Visibility: memory.Private,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	admin := workspaceCall(t, f.admin, "search_memory", map[string]any{"query": "convention", "explain": true}, false)
	if !strings.Contains(string(admin), "another-project") || strings.Contains(string(admin), "two/project") {
		t.Fatalf("admin MCP does not match tenant UI permissions: %s", admin)
	}
	bound := workspaceCall(t, f.bound, "search_memory", map[string]any{"query": "convention"}, false)
	if strings.Contains(string(bound), "another-project") || !strings.Contains(string(bound), "one/project") {
		t.Fatalf("bound key escaped its UI permissions: %s", bound)
	}
	workspaceCall(t, f.bound, "search_memory", map[string]any{"query": "convention", "user_id": "another-project"}, true)
	workspaceCall(t, f.admin, "search_memory", map[string]any{"query": "convention", "tenant": "two"}, true)
	page := workspaceCall(t, f.admin, "list_memories", map[string]any{"limit": 1}, false)
	var first struct {
		Memories []memory.Memory `json:"memories"`
	}
	if err := json.Unmarshal(page, &first); err != nil || len(first.Memories) != 1 {
		t.Fatalf("first page %s %v", page, err)
	}
	id := first.Memories[0].ID
	next := workspaceCall(t, f.admin, "list_memories", map[string]any{"limit": 1, "before_id": id}, false)
	if strings.Contains(string(next), id) {
		t.Fatal("cursor page repeated first memory")
	}
	workspaceCall(t, f.bound, "get_memory", map[string]any{"id": id}, first.Memories[0].User != "project")
	workspaceCall(t, f.other, "get_memory", map[string]any{"id": id}, true)
	info := workspaceCall(t, f.admin, "workspace_info", map[string]any{}, false)
	if !strings.Contains(string(info), `"tenant":"one"`) || !strings.Contains(string(info), `"extraction_schema"`) {
		t.Fatalf("missing workspace discovery %s", info)
	}
	workspaceCall(t, f.admin, "memory_history", map[string]any{"id": id}, false)
	workspaceCall(t, f.admin, "memory_versions", map[string]any{"id": id}, false)
	workspaceCall(t, f.admin, "list_entities", map[string]any{}, false)
	workspaceCall(t, f.admin, "memory_relationships", map[string]any{"id": id, "depth": 2}, false)
	workspaceCall(t, f.admin, "embedding_status", map[string]any{}, false)
}

func TestWorkspaceMutationsRequireConfirmationAndKeepTenantBoundary(t *testing.T) {
	f := newWorkspaceFixture(t)
	raw := workspaceCall(t, f.admin, "remember_memory", map[string]any{"text": "Original approved convention."}, false)
	var changes struct {
		Changes []struct {
			Memory memory.Memory `json:"memory"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(raw, &changes); err != nil || len(changes.Changes) != 1 {
		t.Fatalf("remember response %s %v", raw, err)
	}
	id := changes.Changes[0].Memory.ID
	if changes.Changes[0].Memory.User != "project" {
		t.Fatal("configured MCP write owner lost")
	}
	args := map[string]any{"id": id, "patch": map[string]any{"text": "Corrected approved convention.", "operation_key": "correction-1"}}
	workspaceCall(t, f.admin, "update_memory", args, true)
	args["confirm"] = false
	workspaceCall(t, f.admin, "update_memory", args, true)
	args["confirm"] = true
	updated := workspaceCall(t, f.admin, "update_memory", args, false)
	var m memory.Memory
	if err := json.Unmarshal(updated, &m); err != nil || m.Text != "Corrected approved convention." || m.Supersedes != id {
		t.Fatalf("correction not versioned: %s %v", updated, err)
	}
	workspaceCall(t, f.other, "delete_memory", map[string]any{"id": m.ID, "confirm": true}, true)
	workspaceCall(t, f.admin, "delete_memory", map[string]any{"id": m.ID}, true)
	workspaceCall(t, f.admin, "get_memory", map[string]any{"id": m.ID}, false)
	workspaceCall(t, f.admin, "delete_memory", map[string]any{"id": m.ID, "confirm": true}, false)
	workspaceCall(t, f.admin, "get_memory", map[string]any{"id": m.ID}, true)
	workspaceCall(t, f.bound, "put_settings", map[string]any{"settings": map[string]any{}, "confirm": true}, true)
	workspaceCall(t, f.admin, "put_settings", map[string]any{"settings": map[string]any{"default_policy": "deep"}, "confirm": true}, false)
	if settings := workspaceCall(t, f.admin, "get_settings", map[string]any{}, false); !strings.Contains(string(settings), `"default_policy":"deep"`) {
		t.Fatalf("settings did not persist %s", settings)
	}
	workspaceCall(t, f.admin, "backfill_embeddings", map[string]any{}, true)
	workspaceCall(t, f.admin, "backfill_embeddings", map[string]any{"confirm": true}, false)
}

func TestWorkspaceJobsAreBoundedAdminOnlyAndTenantScoped(t *testing.T) {
	f := newWorkspaceFixture(t)
	for _, tenant := range []string{"one", "two"} {
		_, err := f.store.Write(context.Background(), &store.Batch{Tenant: tenant, Jobs: []store.Job{
			{ID: tenant + "-a", Kind: "fixture", Key: "a"}, {ID: tenant + "-b", Kind: "fixture", Key: "b"},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	page := workspaceCall(t, f.admin, "list_jobs", map[string]any{"kind": "fixture", "limit": 1}, false)
	var out struct {
		Jobs []store.Job `json:"jobs"`
		Next string      `json:"next_after_id"`
	}
	if err := json.Unmarshal(page, &out); err != nil || len(out.Jobs) != 1 || out.Next == "" {
		t.Fatalf("jobs page %s %v", page, err)
	}
	next := workspaceCall(t, f.admin, "list_jobs", map[string]any{"kind": "fixture", "limit": 1, "after_id": out.Next}, false)
	if strings.Contains(string(next), out.Jobs[0].ID) || strings.Contains(string(next), "two-") {
		t.Fatalf("job page repeated or escaped tenant %s", next)
	}
	workspaceCall(t, f.bound, "list_jobs", map[string]any{}, true)
	workspaceCall(t, f.admin, "control_job", map[string]any{"id": "two-a", "action": "cancel", "confirm": true}, true)
	for _, action := range []string{"pause", "resume", "priority", "cancel", "retry", "delete"} {
		args := map[string]any{"id": "one-a", "action": action, "confirm": true}
		if action == "priority" {
			args["priority"] = 60
		}
		workspaceCall(t, f.admin, "control_job", args, false)
	}
	workspaceCall(t, f.admin, "list_jobs", map[string]any{"limit": 101}, true)
	workspaceCall(t, f.admin, "list_jobs", map[string]any{"state": "not-a-state"}, true)
	workspaceCall(t, f.admin, "control_job", map[string]any{"id": "one-b", "action": "priority", "confirm": true}, true)
}

func TestWorkspaceFileImportsAreDurableAndMultimodal(t *testing.T) {
	f := newWorkspaceFixture(t)
	for _, tool := range []string{"import_document", "import_media"} {
		result := workspaceCall(t, f.admin, tool, map[string]any{
			"name": tool + ".txt", "bytes": base64.StdEncoding.EncodeToString([]byte("Generated fixture for " + tool)),
		}, false)
		var imported niromem.DocumentImport
		if err := json.Unmarshal(result, &imported); err != nil || imported.ID == "" || imported.Status != "queued" {
			t.Fatalf("upload did not return durable receipt %s %v", result, err)
		}
		workspaceCall(t, f.admin, "document_status", map[string]any{"id": imported.ID}, false)
		workspaceCall(t, f.other, "document_status", map[string]any{"id": imported.ID}, true)
		kind := niromem.JobDocumentImport
		if tool == "import_media" {
			kind = niromem.JobMediaInterpret
		}
		jobs, err := f.store.Jobs(context.Background(), store.JobQuery{Tenant: "one", Kind: kind, Key: imported.ID})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("missing durable upload job %+v %v", jobs, err)
		}
		if err := f.engine.Worker().Handlers[kind](context.Background(), jobs[0]); err != nil {
			t.Fatal(err)
		}
		status := workspaceCall(t, f.admin, "document_status", map[string]any{"id": imported.ID}, false)
		if !strings.Contains(string(status), `"memories":["chunk-`) {
			t.Fatalf("no extracted source IDs %s", status)
		}
	}
	workspaceCall(t, f.admin, "list_documents", map[string]any{}, false)
	workspaceCall(t, f.admin, "import_document", map[string]any{"name": "x.txt", "bytes": "bad base64"}, true)
	workspaceCall(t, f.admin, "import_document", map[string]any{"name": "x.txt", "path": "/etc/passwd"}, true)
}

func TestWorkspaceCleanupRequiresApprovalExactTenantAndStableReceipt(t *testing.T) {
	f := newWorkspaceFixture(t)
	args := map[string]any{"confirmation": "one", "request_id": "cleanup-1"}
	workspaceCall(t, f.admin, "delete_all_memories", args, true)
	args["confirm"], args["confirmation"] = true, "two"
	workspaceCall(t, f.admin, "delete_all_memories", args, true)
	args["confirmation"] = "one"
	workspaceCall(t, f.bound, "delete_all_memories", args, true)
	first := workspaceCall(t, f.admin, "delete_all_memories", args, false)
	second := workspaceCall(t, f.admin, "delete_all_memories", args, false)
	var a, b store.PurgeStatus
	if json.Unmarshal(first, &a) != nil || json.Unmarshal(second, &b) != nil || a.ID == "" || a.ID != b.ID {
		t.Fatalf("cleanup retry created distinct wipes: %s %s", first, second)
	}
	workspaceCall(t, f.admin, "cleanup_status", map[string]any{}, false)
	if rows, err := f.store.Jobs(context.Background(), store.JobQuery{Tenant: "one", Kind: store.JobTenantPurge}); err != nil || len(rows) != 1 {
		t.Fatalf("cleanup not durable: %+v %v", rows, err)
	}
}

func TestWorkspaceCatalogueAndValidation(t *testing.T) {
	f := newWorkspaceFixture(t)
	seen := map[string]bool{}
	for _, tool := range f.admin.availableTools() {
		def := tool.(map[string]any)
		name := def["name"].(string)
		if seen[name] {
			t.Fatalf("duplicate tool %s", name)
		}
		seen[name] = true
		schema := def["inputSchema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Fatalf("%s accepts arbitrary arguments", name)
		}
	}
	if len(seen) < 30 {
		t.Fatalf("missing workspace capabilities: %v", seen)
	}
	for _, args := range []any{nil, []string{}, map[string]any{"query": ""}, map[string]any{"query": "ok", "limit": 1.5}, map[string]any{"query": "ok", "explain": "true"}} {
		workspaceCall(t, f.admin, "search_memory", args, true)
	}
	for _, name := range []string{"delete_memory", "update_memory", "control_job", "put_settings", "delete_all_memories"} {
		if !seen[name] {
			t.Fatalf("missing guarded tool %s", name)
		}
	}

	if _, err := f.admin.client.(WorkspaceBackend).WorkspaceRequest(context.Background(), http.MethodGet, "/v1/jobs", nil); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceKnowledgeProposalAndConfirmedCommit(t *testing.T) {
	f := newWorkspaceFixture(t)
	args := map[string]any{
		"operation_key": "approved-knowledge-1", "text": "Atlas uses Redis.", "slot": "atlas.database",
		"subject": "Atlas", "predicate": "uses", "object": "Redis",
	}
	proposed := workspaceCall(t, f.admin, "propose_memory_update", args, false)
	var proposal niromem.KnowledgeResult
	if err := json.Unmarshal(proposed, &proposal); err != nil || proposal.Status != "proposed" {
		t.Fatalf("proposal not read-only %s %v", proposed, err)
	}
	workspaceCall(t, f.admin, "commit_memory_update", args, true)
	args["confirm"] = true
	first := workspaceCall(t, f.admin, "commit_memory_update", args, false)
	second := workspaceCall(t, f.admin, "commit_memory_update", args, false)
	var a, b niromem.KnowledgeResult
	if json.Unmarshal(first, &a) != nil || json.Unmarshal(second, &b) != nil || !b.Replayed || len(a.Changes) != 1 || len(b.Changes) != 1 || a.Changes[0].Memory.ID != b.Changes[0].Memory.ID {
		t.Fatalf("commit replay not durable: %s %s", first, second)
	}
}

func TestWorkspaceFileEnvelopeAboveLegacyOneMiBLimit(t *testing.T) {
	f := newWorkspaceFixture(t)
	args := map[string]any{
		"name":  "large-fixture.txt",
		"bytes": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 1<<20))),
	}
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "import_document", "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/mcp", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	f.admin.HTTPHandler("8080", "").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"isError":true`) {
		t.Fatalf("workspace HTTP rejected bounded file upload: %d %s", rec.Code, rec.Body.String())
	}
	var output strings.Builder
	if err := f.admin.ServeStdio(context.Background(), strings.NewReader(string(data)+"\n"), &output); err != nil {
		t.Fatalf("workspace stdio rejected bounded file upload: %v", err)
	}
	if strings.Contains(output.String(), `"isError":true`) {
		t.Fatalf("stdio import failed: %s", output.String())
	}
}
