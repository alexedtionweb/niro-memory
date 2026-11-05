package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
)

// Reads source evidence only; never imports, reprocesses or updates live data.
func TestLocalExistingDocumentExclusionPassage(t *testing.T) {
	doc, tenant := os.Getenv("NIROMEM_TEST_DOCUMENT_ID"), os.Getenv("NIROMEM_TEST_DOCUMENT_TENANT")
	if os.Getenv("NIROMEM_TEST_LOCAL_EXTRACTION") != "1" || doc == "" || tenant == "" {
		t.Skip("opt-in read-only local document source extraction")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	get := func(path string, dst any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:8080"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("read-only source request: HTTP %d", resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatal(err)
		}
	}
	var document struct {
		Memories []string `json:"memories"`
	}
	get("/v1/documents/"+doc, &document)
	if len(document.Memories) < 3 {
		t.Fatal("document has no third passage")
	}
	var source struct {
		Text string `json:"text"`
	}
	get("/v1/memories/"+document.Memories[2], &source)
	noThinking := false
	p, err := New(Options{Protocol: ProtocolOllama, BaseURL: "http://127.0.0.1:11434", Model: "qwen3.5:2b", Think: &noThinking,
		NumThread: 4, NumCtx: 8192, NumPredict: 1536, HTTP: &http.Client{Timeout: 4 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	input := domain.EnrichmentInput{Text: source.Text}
	start := time.Now()
	out, err := p.Enrich(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateEnrichment(input, out); err != nil {
		t.Fatal(err)
	}
	if len(out.Facts) < 2 {
		t.Fatalf("existing document passage still yields insufficient structured records: %d facts, %d issues", len(out.Facts), len(out.Issues))
	}
	t.Logf("Previously failing third passage: %d grounded claims, %d entity annotations, %d explicit issues, %s. Live data unchanged.",
		len(out.Facts), len(out.Entities), len(out.Issues), time.Since(start).Round(time.Millisecond))
}
