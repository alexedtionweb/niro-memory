package mcp

import (
	"context"
	"encoding/json"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func TestEmbeddingStatusReportsTenantAndIndependentCoverage(t *testing.T) {
	f := newWorkspaceFixture(t)
	ctx := context.Background()
	for _, tenant := range []string{"one", "two"} {
		if _, err := f.engine.Remember(ctx, niromem.Fact{Scope: memory.Scope{Tenant: tenant, User: "project"}, Text: "Atlas stores cached data."}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.engine.Worker().Handlers[niromem.JobEmbed](ctx, store.Job{Tenant: "one"}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"one", "two"} {
		server := f.admin
		wantHave := 1
		if tenant == "two" {
			server = f.other
			wantHave = 0
		}
		raw := workspaceCall(t, server, "embedding_status", map[string]any{}, false)
		var status struct {
			Identity struct {
				Tenant string `json:"tenant"`
			} `json:"identity"`
			Model    string                    `json:"model"`
			Coverage struct{ Have, Total int } `json:"coverage"`
			Scope    string                    `json:"coverage_scope"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatal(err)
		}
		have, total, err := f.engine.Coverage(ctx, tenant, status.Model)
		if err != nil || status.Identity.Tenant != tenant || status.Coverage.Have != wantHave || status.Coverage.Total != 1 ||
			status.Coverage.Have != have || status.Coverage.Total != total || status.Scope == "" {
			t.Fatalf("MCP coverage or identity differs from native engine: %s (%v)", raw, err)
		}
	}
}
