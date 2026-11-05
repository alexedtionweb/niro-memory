package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
)

func TestNumericLossGetsFocusedCorrectionAndCannotHideOtherWindows(t *testing.T) {
	for _, repaired := range []bool{false, true} {
		t.Run(fmt.Sprint(repaired), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request struct {
					Messages []map[string]string `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				source := request.Messages[len(request.Messages)-1]["content"]
				if strings.Contains(source, "Atlas") {
					writeOllamaPlan(w, `{"facts":[{"evidence_id":0,"subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`)
					return
				}
				object := "Signal"
				if requests == 2 {
					if !strings.Contains(request.Messages[0]["content"], "75%") || !strings.Contains(request.Messages[0]["content"], "missing from structured assertions") {
						t.Error("correction lacks specific lost-value feedback")
					}
					if repaired {
						object = "at least 75%"
					}
				}
				writeOllamaPlan(w, fmt.Sprintf(`{"facts":[{"evidence_id":0,"subject":"Signal","predicate":"requires","object":%q,"type":"fact"}],"entities":[],"relations":[]}`, object))
			}))
			defer server.Close()
			c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Signal must be at least 75%. Atlas uses Redis."})
			if err != nil || requests != 3 {
				t.Fatalf("unbounded correction: %+v requests=%d error=%v", out, requests, err)
			}
			if repaired {
				if len(out.Facts) != 2 || len(out.Issues) != 0 || out.Facts[0].Object != "at least 75%" {
					t.Fatalf("correction lost actual source limit: %+v", out)
				}
			} else if len(out.Facts) != 1 || out.Facts[0].Subject != "Atlas" || len(out.Issues) != 1 || !strings.Contains(out.Issues[0].Reason, "75%") {
				t.Fatalf("numeric loss hid healthy source or was silently accepted: %+v", out)
			}
		})
	}
}

func TestExtractionDetectsNumericLossWithoutInventingValues(t *testing.T) {
	for _, tc := range []struct {
		quote, object string
		missing       bool
	}{
		{"Signal must be at least 75%.", "at least 75%", false},
		{"Signal must be at least 75%.", "Signal", true},
		{"Signal must be at least 75%.", "at least 175%", true},
		{"Signal must be at least 75%.", "at least 75", true},
		{"Signal must be at least 75 %.", "at least 75%", false},
		{"Session expires after 45 minutes.", "45 minutes", false},
		{"Session expires after 45 minutes.", "minutes", true},
	} {
		out := domain.Enrichment{Facts: []domain.ExtractedFact{{Quote: tc.quote, Subject: "Signal", Predicate: "requires", Object: tc.object}}}
		if got := len(omittedNumericValues(out)) > 0; got != tc.missing {
			t.Errorf("numeric coverage for %q -> %q: missing=%t, want %t", tc.quote, tc.object, got, tc.missing)
		}
	}
}

func TestDocumentWindowsBoundWorkAndReportUnprocessedEvidence(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		var source struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(request.Messages[len(request.Messages)-1]["content"]), &source); err != nil {
			t.Fatal(err)
		}
		writeOllamaPlan(w, fmt.Sprintf(`{"facts":[{"evidence_id":0,"subject":%q,"predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`, strings.Fields(source.Text)[0]))
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for i := range 20 {
		fmt.Fprintf(&text, "Item%d uses Redis. ", i)
	}
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: text.String()})
	if err != nil || requests != 16 || len(out.Facts) != 16 || len(out.Issues) != 1 || out.Issues[0].Stage != "coverage" || out.Issues[0].Index != 16 {
		t.Fatalf("unbounded work or silent loss of remaining evidence: %+v requests=%d error=%v", out, requests, err)
	}
}

func TestDocumentWindowsPreserveHealthyRecordsAroundInvalidUnit(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		var source struct {
			Text     string         `json:"text"`
			Evidence []evidenceSpan `json:"evidence"`
		}
		if err := json.Unmarshal([]byte(request.Messages[len(request.Messages)-1]["content"]), &source); err != nil {
			t.Fatal(err)
		}
		if len(source.Evidence) > 1 {
			t.Fatal("dense source was not decomposed")
		}
		if strings.Contains(source.Text, "Cora") || strings.Contains(source.Text, "Dana") {
			writeOllamaPlan(w, `{"facts":[{"evidence_id":0,"subject":"Invented","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`)
			return
		}
		var facts []map[string]any
		for i, span := range source.Evidence {
			facts = append(facts, map[string]any{"evidence_id": i, "subject": strings.Fields(span.Text)[0], "predicate": "uses", "object": "Redis", "type": "fact", "subject_type": "", "object_type": "", "negated": false})
		}
		content, err := json.Marshal(map[string]any{"facts": facts, "entities": []any{}, "relations": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		writeOllamaPlan(w, string(content))
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	input := domain.EnrichmentInput{Text: "Atlas uses Redis. Bruna uses Redis. Cora uses Redis. Dana uses Redis. Elio uses Redis. Fara uses Redis."}
	out, err := c.Enrich(context.Background(), input)
	if err != nil || len(out.Facts) != 4 || len(out.Issues) != 2 || out.Issues[0].Stage != "window" || requests != 8 {
		t.Fatalf("window recovery: %+v requests=%d error=%v", out, requests, err)
	}
	if out.Facts[0].Subject != "Atlas" || out.Facts[3].Subject != "Fara" {
		t.Fatal("healthy windows were lost")
	}
	if err := domain.ValidateEnrichment(input, out); err != nil {
		t.Fatal(err)
	}
}

func TestWindowTransportFailureRetainsCompletedExtraction(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 1 {
			http.Error(w, "unavailable", 503)
			return
		}
		writeOllamaPlan(w, `{"facts":[{"evidence_id":0,"subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}],"entities":[],"relations":[]}`)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis. Bruna uses Redis. Cora uses Redis. Dana uses Redis."})
	if err != nil || len(out.Facts) != 1 || len(out.Issues) != 1 || out.Issues[0].Stage != "provider" || requests != 2 {
		t.Fatalf("transport lost validated work: %+v requests=%d error=%v", out, requests, err)
	}
}
