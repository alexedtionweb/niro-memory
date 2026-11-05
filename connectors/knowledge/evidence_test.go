package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

func TestSourceEvidenceOffsetsPreserveRepeatedAndUnicodeSentences(t *testing.T) {
	text := "  Atlas usa Redis.\n\nAtlas usa Redis.  Bruna prefere chá.\n"
	spans := sourceEvidence(text)
	if len(spans) != 3 || spans[0].start == spans[1].start {
		t.Fatalf("repeated source offsets lost: %+v", spans)
	}
	for _, span := range spans {
		if text[span.start:span.end] != span.Text {
			t.Fatalf("window is not its original UTF-8 byte slice: %+v", span)
		}
	}
}

func TestIndexedEvidenceExtractionAcrossProvidersAndModels(t *testing.T) {
	for _, protocol := range []string{ProtocolOllama, ProtocolOpenAI} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []map[string]string `json:"messages"`
					Format   json.RawMessage     `json:"format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				var source struct {
					Text     string         `json:"text"`
					Evidence []evidenceSpan `json:"evidence"`
				}
				if err := json.Unmarshal([]byte(request.Messages[len(request.Messages)-1]["content"]), &source); err != nil {
					t.Error(err)
				}
				if len(source.Evidence) != 1 || source.Evidence[0].ID != 0 || !strings.Contains(source.Text, source.Evidence[0].Text) {
					t.Errorf("missing deterministic source evidence: %+v", source)
				}
				for _, message := range request.Messages {
					if message["role"] == "assistant" && strings.HasPrefix(message["content"], "{") {
						if strings.Contains(message["content"], `"quote"`) {
							t.Error("examples still ask model to reproduce quotation text")
						}
					}
				}
				if protocol == ProtocolOllama {
					var schema map[string]any
					if err := json.Unmarshal(request.Format, &schema); err != nil {
						t.Error(err)
					}
					fact := schema["properties"].(map[string]any)["facts"].(map[string]any)["items"].(map[string]any)
					properties := fact["properties"].(map[string]any)
					if properties["quote"] != nil || properties["evidence_id"].(map[string]any)["maximum"] != float64(0) {
						t.Errorf("model output is not constrained to source handles: %s", request.Format)
					}
				}
				content := `{"facts":[{"evidence_id":0,"subject":"Bruna","subject_type":"person","predicate":"works at","object":"Contoso","object_type":"organization","type":"fact","negated":false}],"entities":[{"name":"Bruna","type":"person","evidence_id":0}],"relations":[]}`
				if !strings.Contains(source.Text, "Bruna") {
					content = `{"facts":[],"entities":[],"relations":[]}`
				}
				if protocol == ProtocolOllama {
					writeOllamaPlan(w, content)
				} else {
					writeOpenAIPlan(w, content)
				}
			}))
			defer server.Close()
			c, err := New(Options{Protocol: protocol, BaseURL: server.URL, Model: "arbitrary-model-not-hardcoded"})
			if err != nil {
				t.Fatal(err)
			}
			source := "Unrelated background.\nBruna trabalha na Contoso."
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: source})
			if err != nil || len(out.Facts) != 1 || out.Facts[0].Quote != "Bruna trabalha na Contoso." || len(out.Entities) != 2 {
				t.Fatalf("indexed extraction failed: %+v %v", out, err)
			}
			if err := domain.ValidateEnrichment(domain.EnrichmentInput{Text: source}, out); err != nil {
				t.Fatalf("resolved evidence did not pass unchanged validator: %v", err)
			}
		})
	}
}

func TestEvidenceReferencesCannotBypassGrounding(t *testing.T) {
	for name, content := range map[string]string{
		"unknown-reference":    `{"facts":[{"evidence_id":99,"subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`,
		"negative-reference":   `{"facts":[{"evidence_id":-1,"subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`,
		"invented-object":      `{"facts":[{"evidence_id":0,"subject":"Atlas","predicate":"uses","object":"MongoDB","type":"fact"}],"entities":[],"relations":[]}`,
		"conflicting-quote":    `{"facts":[{"evidence_id":0,"quote":"Atlas uses MongoDB.","subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`,
		"invented-entity":      `{"facts":[],"entities":[{"name":"MongoDB","type":"database","evidence_id":0}],"relations":[]}`,
		"candidate-reference":  `{"facts":[{"evidence_id":1,"subject":"Other","predicate":"uses","object":"MongoDB","type":"fact"}],"entities":[],"relations":[]}`,
		"fractional-reference": `{"facts":[{"evidence_id":0.5,"subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				writeOllamaPlan(w, content)
			}))
			defer server.Close()
			c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Enrich(context.Background(), domain.EnrichmentInput{
				Text: "Atlas uses Redis.", Candidates: []memory.Memory{{Text: "Other uses MongoDB."}},
			})
			if !errors.Is(err, memory.ErrInvalid) || requests != 2 {
				t.Fatalf("fabrication accepted or retry unbounded: requests=%d error=%v", requests, err)
			}
		})
	}
}

func TestSourceEvidencePreservesExactUTF8AndBounds(t *testing.T) {
	source := "  Bruna usa chá! \nAtlas does not use Redis. Question?\n" + strings.Repeat("界", 1800)
	spans := sourceEvidence(source)
	if len(spans) < 4 || spans[1].Text != "Atlas does not use Redis." || spans[2].Text != "Question?" {
		t.Fatalf("sentence evidence segmentation incorrect: %+v", spans[:min(len(spans), 3)])
	}
	for i, span := range spans {
		if span.ID != i || span.Text == "" || len(span.Text) > 2048 || !utf8.ValidString(span.Text) || !strings.Contains(source, span.Text) {
			t.Fatalf("evidence changed or lost source bounds at %d", i)
		}
	}
	if got := sourceEvidence(" \n\t"); len(got) != 0 {
		t.Fatalf("whitespace became evidence: %+v", got)
	}
}
