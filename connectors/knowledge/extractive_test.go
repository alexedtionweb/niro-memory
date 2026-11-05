package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

func TestExtractiveSelectionAcrossProviders(t *testing.T) {
	for _, protocol := range []string{ProtocolOllama, ProtocolOpenAI} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					Messages []map[string]string `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if len(request.Messages) != 2 || strings.Contains(request.Messages[1]["content"], "candidate-marker") {
					t.Error("selection is not a minimal source-only request")
				}
				content := `{"ids":[1,0]}`
				if protocol == ProtocolOllama {
					writeOllamaPlan(w, content)
				} else {
					writeOpenAIPlan(w, content)
				}
			}))
			defer server.Close()
			c, err := New(Options{Protocol: protocol, BaseURL: server.URL, Model: "any-small-model", ExtractiveOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			input := domain.EnrichmentInput{Text: "Atlas uses Redis.\nEU PREFIRO CHÁ GELADO", Candidates: []memory.Memory{{Text: "candidate-marker"}}}
			out, err := c.Enrich(context.Background(), input)
			if err != nil || calls != 1 || !reflect.DeepEqual(out.Excerpts, []string{"EU PREFIRO CHÁ GELADO", "Atlas uses Redis."}) || len(out.Facts) != 0 || len(out.Entities) != 0 || len(out.Relations) != 0 {
				t.Fatalf("selection: %+v %v", out, err)
			}
			if err := domain.ValidateEnrichment(input, out); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtractiveInvalidSelectionNeedsReview(t *testing.T) {
	for name, content := range map[string]string{
		"malformed":    `{`,
		"missing":      `{}`,
		"null":         `{"ids":null}`,
		"fraction":     `{"ids":[0.5]}`,
		"string":       `{"ids":["0"]}`,
		"negative":     `{"ids":[-1]}`,
		"out-of-range": `{"ids":[1]}`,
		"duplicate":    `{"ids":[0,0]}`,
		"extra-fields": `{"ids":[0],"text":"invented"}`,
		"trailing":     `{"ids":[0]} {}`,
		"too-many":     `{"ids":[0,0,0,0,0]}`,
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; writeOllamaPlan(w, content) }))
			defer server.Close()
			c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "test", ExtractiveOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
			var review *domain.ExtractionReviewRequired
			if !errors.As(err, &review) || calls != 1 || len(out.Excerpts) > 0 {
				t.Fatalf("invalid response must require review without retries or partial data: %+v calls=%d %v", out, calls, err)
			}
		})
	}
}

func TestExtractiveTransportFailureRemainsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "test", ExtractiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	var review *domain.ExtractionReviewRequired
	if err == nil || errors.As(err, &review) {
		t.Fatalf("transport error hidden: %v", err)
	}
	if _, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "test", ExtractiveOnly: true, StagedExtraction: true}); err == nil {
		t.Fatal("conflicting extraction modes accepted")
	}
}

func TestExtractiveEmptyAndRepeatedSourceText(t *testing.T) {
	for _, selection := range []string{`{"ids":[]}`, `{"ids":[0,1]}`} {
		t.Run(selection, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeOllamaPlan(w, selection) }))
			defer server.Close()
			c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "test", ExtractiveOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis. Atlas uses Redis."})
			want := 0
			if selection != `{"ids":[]}` {
				want = 1
			}
			if err != nil || len(out.Excerpts) != want || out.Facts == nil || out.Relations == nil {
				t.Fatalf("invalid empty/dedup result: %+v %v", out, err)
			}
		})
	}
}
