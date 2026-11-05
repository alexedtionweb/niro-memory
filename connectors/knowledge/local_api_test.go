package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Only generated fixture text is sent to the explicitly enabled local model.
func TestLocalModelExtractionThroughAPI(t *testing.T) {
	if os.Getenv("NIROMEM_TEST_LOCAL_EXTRACTION") != "1" {
		t.Skip("set NIROMEM_TEST_LOCAL_EXTRACTION=1 to call local Ollama with generated fixtures")
	}
	model := os.Getenv("NIROMEM_TEST_EXTRACTION_MODEL")
	if model == "" {
		model = "LiquidAI/lfm2.5-350m"
	}
	var lastResponse string
	httpClient := &http.Client{Timeout: 10 * time.Minute, Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		lastResponse = body.Message.Content
		return resp, nil
	})}
	planner, err := New(Options{Protocol: ProtocolOllama, BaseURL: "http://127.0.0.1:11434", Model: model,
		ExtractiveOnly: true,
		NumThread:      4, NumCtx: 4096, NumPredict: 1536, HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	s := memstore.New(nil)
	e, err := niromem.Open(niromem.Options{Store: s, Knowledge: niromem.KnowledgeOptions{
		Mode: "enforced", Planner: planner, BackgroundEnrichment: true, Timeout: 10 * time.Minute,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	server := httptest.NewServer((&api.Server{Engine: e, Auth: api.Dev{}}).Handler())
	defer server.Close()
	c := client.New(server.URL, "")
	for _, text := range []string{"Atlas uses Redis.", "Bruna trabalha na Contoso em Recife.", "EU PREFIRO CHÁ GELADO",
		"Atlas uses Redis. Bruna works at Contoso. We prefer tea."} {
		t.Run(text, func(t *testing.T) {
			lastResponse = ""
			defer func() {
				if t.Failed() {
					t.Logf("last model response: %.800s", lastResponse)
				}
			}()
			changes, err := c.Remember(context.Background(), niromem.Fact{Scope: memory.Scope{User: "generated-fixtures"}, Text: text})
			if err != nil || len(changes) != 1 {
				t.Fatalf("API save: %+v %v", changes, err)
			}

			id := changes[0].Memory.ID
			rows, err := s.Jobs(context.Background(), store.JobQuery{Tenant: "dev", Kind: niromem.JobKnowledgeEnrich, Key: id})
			if err != nil || len(rows) != 1 {
				t.Fatalf("source job: %+v %v", rows, err)
			}
			if err := e.Worker().Handlers[niromem.JobKnowledgeEnrich](context.Background(), rows[0]); err != nil {
				t.Fatal(err)
			}
			raw, err := c.WorkspaceRequest(context.Background(), "GET", "/v1/knowledge/enrichment/"+id, nil)
			if err != nil {
				t.Fatal(err)
			}
			var result niromem.EnrichmentResult
			if err := json.Unmarshal(raw, &result); err != nil || result.Status != "completed" || result.Extraction == nil || len(result.Extraction.Excerpts) == 0 {
				t.Fatalf("API did not return grounded extraction: %s %v", raw, err)
			}
			spans := sourceEvidence(text)
			for _, excerpt := range result.Extraction.Excerpts {
				found := false
				for _, span := range spans {
					found = found || span.Text == excerpt
				}
				if !found {
					t.Fatalf("excerpt is not a supplied source span: %q", excerpt)
				}
			}
			if len(spans) == 1 && result.Extraction.Excerpts[0] != text || len(result.Extraction.Facts) != 0 || len(result.Extraction.Entities) != 0 || len(result.Extraction.Relations) != 0 || len(result.Relations) != 0 {
				t.Fatalf("extractive output changed text or invented structured knowledge: %+v", result)
			}
			t.Logf("API completed: %d exact source excerpts; no structured claims or graph", len(result.Extraction.Excerpts))
		})
	}
}
