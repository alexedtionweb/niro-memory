package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/blob"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/embed"
	"github.com/alexedtionweb/niro-memory/jobs"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
)

func TestLocalDocumentRevisionThroughAPI(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EXTRACTION") != "1" || os.Getenv("NIROMEM_TEST_LOCAL_EMBEDDING") != "1" {
		t.Skip("opt-in Qwen + EmbeddingGemma document revision API test; generated fixture and temporary SQLite")
	}
	testDocumentRevisionThroughSQLiteAPI(t, true, false)
}

func TestDocumentRevisionThroughSQLiteAPI(t *testing.T) {
	testDocumentRevisionThroughSQLiteAPI(t, false, false)
}

func TestDocumentPartialRepairThroughSQLiteAPI(t *testing.T) {
	testDocumentRevisionThroughSQLiteAPI(t, false, true)
}

type sqliteRevisionPlanner struct {
	knowledge.Deterministic
	calls   int
	partial bool
}

func (p *sqliteRevisionPlanner) Enrich(context.Context, knowledge.EnrichmentInput) (knowledge.Enrichment, error) {
	p.calls++
	result := knowledge.Enrichment{Facts: []knowledge.ExtractedFact{{Quote: "Atlas uses Redis.", Subject: "Atlas", Predicate: "uses", Object: "Redis", Type: memory.EpistemicFact, SubjectType: "project", ObjectType: "database"}}, Relations: []knowledge.ExtractedRelation{}, Provider: "fixture", Model: "structured"}
	if p.partial {
		if p.calls == 1 {
			result.Issues = []knowledge.ExtractionIssue{{Stage: "window", Index: 1, Reason: "Fixture: second window failed validation."}}
		} else {
			result.Facts = []knowledge.ExtractedFact{{Quote: "Bruna works at Contoso.", Subject: "Bruna", Predicate: "works at", Object: "Contoso", Type: memory.EpistemicFact, SubjectType: "person", ObjectType: "organization"}}
		}
	}
	return result, nil
}

func testDocumentRevisionThroughSQLiteAPI(t *testing.T, realModels, partialRepair bool) {
	t.Helper()
	cfg, err := loadConfig("../../configs/lfm2.5-350m.example.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "revisions.db")})
	if err != nil {
		t.Fatal(err)
	}
	isolated := &Config{Store: Choice{Use: "sqlite", Options: map[string]json.RawMessage{"sqlite": raw}}, Embedder: cfg.Embedder, Knowledge: cfg.Knowledge}
	if !realModels {
		isolated.Embedder, isolated.Knowledge = Choice{}, KnowledgeConfig{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var options niromem.Options
	closers, err := connectors(ctx, isolated, &options)
	for _, closer := range closers {
		defer closer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	planner := &sqliteRevisionPlanner{partial: partialRepair}
	if !realModels {
		options.Embedder = embed.Hash{Dims: 8}
		options.Knowledge = niromem.KnowledgeOptions{Mode: "enforced", Planner: planner}
	}
	options.Documents, options.Blob = retrievalDocumentParser{}, blob.NewMem()
	e, err := niromem.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer((&api.Server{Engine: e, Auth: api.Dev{}}).Handler())
	defer srv.Close()
	c := client.New(srv.URL, "")
	sourceText := "Atlas uses Redis."
	if partialRepair {
		sourceText += " Bruna works at Contoso."
	}
	raw, err = c.WorkspaceRequest(ctx, "POST", "/v1/documents", map[string]any{"name": "generated-fixture.txt", "bytes": []byte(sourceText), "user_id": "generated"})
	if err != nil {
		t.Fatal(err)
	}
	var doc niromem.DocumentImport
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	js, err := options.Store.Jobs(ctx, store.JobQuery{Tenant: "dev", Kind: niromem.JobDocumentImport})
	if err != nil || len(js) != 1 {
		t.Fatalf("import jobs: %v %v", js, err)
	}
	if err := e.Worker().Handlers[niromem.JobDocumentImport](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	raw, err = c.WorkspaceRequest(ctx, "POST", "/v1/documents/"+doc.ID+"/reprocess", niromem.DocumentReprocessRequest{RequestID: "real-model-revision", Stages: []string{"knowledge", "embeddings"}})
	if err != nil {
		t.Fatal(err)
	}
	var revision niromem.DocumentRevision
	if err := json.Unmarshal(raw, &revision); err != nil {
		t.Fatal(err)
	}
	js, err = options.Store.Jobs(ctx, store.JobQuery{Tenant: "dev", Kind: niromem.JobDocumentReprocess})
	if err != nil || len(js) != 1 {
		t.Fatalf("revision job: %v %v", js, err)
	}
	start := time.Now()
	for attempt := 0; attempt < 5; attempt++ {
		err = e.Worker().Handlers[niromem.JobDocumentReprocess](ctx, js[0])
		if err == nil {
			break
		}
		if !errors.Is(err, jobs.Yield()) {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err = c.WorkspaceRequest(ctx, "GET", "/v1/documents/"+doc.ID+"/graph", nil)
	if err != nil {
		t.Fatal(err)
	}
	var graph niromem.DocumentGraph
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatal(err)
	}
	if graph.Revision.ID != revision.ID || !graph.IsCurrent || graph.TotalChunks != 1 || graph.TotalEnriched < 1 {
		t.Fatalf("real revision did not commit structured knowledge: %s", raw)
	}
	embeddings, links := 0, 0
	for _, node := range graph.Nodes {
		if node.Type == "embedding" {
			embeddings++
			if node.Dims != options.Embedder.Model().Dims {
				t.Fatalf("wrong embedding shape: %+v", node)
			}
		}
	}
	for _, edge := range graph.Edges {
		if edge.Relation == "derived_from" {
			links++
		}
	}
	if embeddings != graph.TotalChunks+graph.TotalEnriched || links == 0 {
		t.Fatalf("missing actual vectors or provenance: %s", raw)
	}
	t.Logf("Native API + SQLite (real models=%t): %d chunk, %d structured items, %d persisted %dd embeddings, %d derived_from links in %s",
		realModels, graph.TotalChunks, graph.TotalEnriched, embeddings, options.Embedder.Model().Dims, links, time.Since(start).Round(time.Millisecond))
	if partialRepair {
		if !graph.Revision.Incomplete || graph.Revision.ExtractedFacts != 1 || graph.Revision.RepairRevisionID == "" {
			t.Fatalf("partial SQLite revision diagnostics lost: %+v", graph.Revision)
		}
		repairs, err := options.Store.Jobs(ctx, store.JobQuery{Tenant: "dev", Kind: niromem.JobDocumentRepair})
		if err != nil || len(repairs) != 1 {
			t.Fatalf("durable repair job: %+v %v", repairs, err)
		}
		for range 2 {
			if err := e.Worker().Handlers[niromem.JobDocumentRepair](ctx, repairs[0]); err != nil {
				t.Fatal(err)
			}
		}
		work, err := options.Store.Jobs(ctx, store.JobQuery{Tenant: "dev", Kind: niromem.JobDocumentReprocess, Key: graph.Revision.RepairRevisionID})
		if err != nil || len(work) != 1 {
			t.Fatalf("repair successor job: %+v %v", work, err)
		}
		for range 2 {
			if err := e.Worker().Handlers[niromem.JobDocumentReprocess](ctx, work[0]); err != nil {
				t.Fatal(err)
			}
		}
		repaired, err := e.DocumentGraph(ctx, "dev", memory.Principal{Admin: true}, doc.ID, "", "", 20)
		if err != nil || repaired.Revision.Incomplete || repaired.TotalEnriched != 2 || repaired.Revision.ExtractedFacts != 2 || planner.calls != 2 {
			t.Fatalf("repair did not preserve partial facts/counts or was not idempotent: %+v calls=%d %v", repaired, planner.calls, err)
		}
		for _, node := range repaired.Nodes {
			if node.Type != "chunk" {
				continue
			}
			annotations, err := json.Marshal(node.Metadata["extracted_entities"])
			if err != nil {
				t.Fatal(err)
			}
			var entities []knowledge.ExtractedEntity
			if err := json.Unmarshal(annotations, &entities); err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, entity := range entities {
				names[entity.Name] = true
			}
			if !names["Redis"] || !names["Contoso"] {
				t.Fatalf("repair erased healthy entity annotations: %s", annotations)
			}
		}
		old, err := e.DocumentRevision(ctx, "dev", memory.Principal{Admin: true}, doc.ID, revision.ID)
		if err != nil || !old.Incomplete || old.ExtractedFacts != 1 {
			t.Fatalf("repair rewrote historical revision: %+v %v", old, err)
		}
		revision = repaired.Revision
	}
	first := revision.ID
	raw, err = c.WorkspaceRequest(ctx, "POST", "/v1/documents/"+doc.ID+"/reprocess", niromem.DocumentReprocessRequest{RequestID: "copy-structured-revision", Stages: []string{"embeddings"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &revision); err != nil {
		t.Fatal(err)
	}
	js, err = options.Store.Jobs(ctx, store.JobQuery{Tenant: "dev", Kind: niromem.JobDocumentReprocess, Key: revision.ID})
	if err != nil || len(js) != 1 {
		t.Fatalf("second revision: %v %v", js, err)
	}
	if err := e.Worker().Handlers[niromem.JobDocumentReprocess](ctx, js[0]); err != nil {
		t.Fatal(err)
	}
	raw, err = c.WorkspaceRequest(ctx, "GET", "/v1/documents/"+doc.ID+"/graph", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatal(err)
	}
	if graph.Revision.ID != revision.ID || graph.TotalEnriched < 1 {
		t.Fatalf("copied knowledge lost: %s", raw)
	}
	expectedCalls := 1
	if partialRepair {
		expectedCalls = 2
	}
	if !realModels && planner.calls != expectedCalls {
		t.Fatalf("unselected model called %d times", planner.calls)
	}
	raw, err = c.WorkspaceRequest(ctx, "GET", "/v1/documents/"+doc.ID+"/graph?revision_id="+first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &graph); err != nil || graph.IsCurrent || graph.TotalEnriched < 1 {
		t.Fatalf("SQLite history lost: %s %v", raw, err)
	}
	request, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/documents/"+doc.ID+"/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := srv.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err = io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(raw) != sourceText {
		t.Fatalf("original download changed: %q %v", raw, err)
	}
}
