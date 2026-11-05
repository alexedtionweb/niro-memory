package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

func TestSourceOnlyRetryPreservesOriginalCandidateIndexes(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		context := body.Messages[len(body.Messages)-3]["content"]
		if requests == 1 {
			if !strings.Contains(context, "Atlas uses Redis.") {
				t.Error("initial context missing")
			}
			writeOllamaPlan(w, `{"facts":[{"quote":"invented quote","subject":"Atlas","predicate":"uses","object":"Redis","type":"fact","negated":false}],"entities":[],"relations":[]}`)
		} else {
			if context != `{"candidates":[]}` {
				t.Errorf("retry retained candidate contamination: %s", context)
			}
			writeOllamaPlan(w, `{"facts":[{"quote":"Atlas does not use Redis.","subject":"Atlas","predicate":"uses","object":"Redis","type":"fact","negated":true}],"entities":[],"relations":[]}`)
		}
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas does not use Redis.",
		Candidates: []memory.Memory{{Text: "Other context"}, {Text: "Atlas uses Redis.", Metadata: map[string]any{
			"assertion": memory.Assertion{Subject: "Atlas", Predicate: "uses", Object: "Redis",
				EpistemicType: memory.EpistemicFact, Confidence: 1, Conditions: map[string]string{"polarity": "positive"}},
		}}}})
	if err != nil || requests != 2 || len(out.Notes) != 1 || len(out.Facts) != 1 ||
		len(out.Relations) != 1 || out.Relations[0].Candidate != 1 || out.Relations[0].Relation != "contradicts" {
		t.Fatalf("source-only retry lost provenance or candidate binding: %+v requests=%d error=%v", out, requests, err)
	}
}

func TestExtractionRepairsInvalidSourceOnlyButDoesNotRetryTransportFailure(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-context", true: "provider-error"}[transportFailure], func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if transportFailure {
					http.Error(w, "overloaded", http.StatusServiceUnavailable)
					return
				}
				writeOllamaPlan(w, `{"facts":[{"quote":"invented","subject":"x","predicate":"uses","object":"y","type":"fact"}],"entities":[],"relations":[]}`)
			}))
			defer server.Close()
			c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			input := domain.EnrichmentInput{Text: "Atlas uses Redis."}
			if transportFailure {
				input.Candidates = []memory.Memory{{Text: "optional context"}}
			}
			_, err = c.Enrich(context.Background(), input)
			wantRequests := 2
			if transportFailure {
				wantRequests = 1
			}
			if err == nil || requests != wantRequests || !transportFailure && !errors.Is(err, memory.ErrInvalid) {
				t.Fatalf("unnecessary CPU retry: requests=%d error=%v", requests, err)
			}
		})
	}
}
func TestSourceOnlyInvalidEntityDoesNotDiscardGroundedFactOrRepeatModel(t *testing.T) {
	for _, protocol := range []string{ProtocolOllama, ProtocolOpenAI} {
		t.Run(protocol, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request struct {
					Messages []map[string]string `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				response := `{"facts":[{"evidence_id":0,"subject":"venta del equipo","subject_type":"","predicate":"se gestiona mediante","object":"esta funcionalidad","object_type":"","type":"fact","negated":true}],"entities":[{"name":"","type":"other","evidence_id":0}],"relations":[]}`
				if requests == 2 {
					if !strings.Contains(request.Messages[0]["content"], "entity 0 needs a name") {
						t.Error("repair did not receive validation feedback")
					}
					response = `{"facts":[{"evidence_id":0,"subject":"venta del equipo","subject_type":"","predicate":"se gestiona mediante","object":"esta funcionalidad","object_type":"","type":"fact","negated":true}],"entities":[],"relations":[]}`
				}
				if protocol == ProtocolOllama {
					writeOllamaPlan(w, response)
				} else {
					writeOpenAIPlan(w, response)
				}
			}))
			defer server.Close()
			c, err := New(Options{Protocol: protocol, BaseURL: server.URL, Model: "small-model"})
			if err != nil {
				t.Fatal(err)
			}
			input := domain.EnrichmentInput{Text: "La venta del equipo no se gestiona mediante esta funcionalidad."}
			out, err := c.Enrich(context.Background(), input)
			if err != nil || requests != 1 || len(out.Facts) != 1 || !out.Facts[0].Negated || len(out.Entities) != 0 || len(out.Issues) != 1 {
				t.Fatalf("grounded repair: %+v requests=%d error=%v", out, requests, err)
			}
		})
	}
}
