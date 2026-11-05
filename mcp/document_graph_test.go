package mcp

import (
	"context"
	"encoding/json"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func TestDocumentLineageAndManualRevisionWorkspaceParity(t *testing.T) {
	f := newWorkspaceFixture(t)
	ctx := context.Background()
	doc, err := f.engine.ImportDocument(ctx, memory.Scope{Tenant: "one", User: "project"}, "manual.txt", []byte("Atlas uses Redis."))
	if err != nil {
		t.Fatal(err)
	}
	js, err := f.store.Jobs(ctx, store.JobQuery{Tenant: "one", Kind: niromem.JobDocumentImport})
	if err != nil || len(js) != 1 {
		t.Fatalf("import job: %v %v", js, err)
	}
	if err := f.engine.Worker().Handlers[niromem.JobDocumentImport](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"id": doc.ID}
	raw := workspaceCall(t, f.bound, "document_graph", args, false)
	var graph niromem.DocumentGraph
	if err := json.Unmarshal(raw, &graph); err != nil || graph.Document.ID != doc.ID || graph.TotalChunks != 1 || len(graph.Nodes) != 2 {
		t.Fatalf("baseline graph: %s %v", raw, err)
	}
	workspaceCall(t, f.other, "document_graph", args, true)
	workspaceCall(t, f.bound, "document_revisions", args, false)
	workspaceCall(t, f.bound, "memory_document", map[string]any{"id": graph.Nodes[1].MemoryID}, false)
	args["request_id"], args["stages"] = "mcp-revision", []string{"embeddings"}
	workspaceCall(t, f.admin, "reprocess_document", args, true)
	args["confirm"] = false
	workspaceCall(t, f.admin, "reprocess_document", args, true)
	args["confirm"] = true
	workspaceCall(t, f.bound, "reprocess_document", args, true)
	workspaceCall(t, f.other, "reprocess_document", args, true)
	raw = workspaceCall(t, f.admin, "reprocess_document", args, false)
	var revision niromem.DocumentRevision
	if err := json.Unmarshal(raw, &revision); err != nil || revision.State != "queued" {
		t.Fatalf("queue: %s %v", raw, err)
	}
	js, err = f.store.Jobs(ctx, store.JobQuery{Tenant: "one", Kind: niromem.JobDocumentReprocess})
	if err != nil || len(js) != 1 {
		t.Fatalf("revision job: %v %v", js, err)
	}
	if err := f.engine.Worker().Handlers[niromem.JobDocumentReprocess](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	raw = workspaceCall(t, f.admin, "document_revisions", map[string]any{"id": doc.ID, "revision_id": revision.ID}, false)
	if err := json.Unmarshal(raw, &revision); err != nil || revision.State != "committed" || revision.Progress == nil {
		t.Fatalf("persisted status: %s %v", raw, err)
	}
	raw = workspaceCall(t, f.bound, "document_graph", map[string]any{"id": doc.ID}, false)
	if err := json.Unmarshal(raw, &graph); err != nil || graph.Revision.ID != revision.ID || len(graph.Nodes) != 3 || !graph.IsCurrent {
		t.Fatalf("MCP/native parity: %s %v", raw, err)
	}
}
