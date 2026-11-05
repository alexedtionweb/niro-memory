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

func stagedFixture(t *testing.T, protocol string, responses []string, check func(int, []map[string]string)) (*Connector, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		index := calls
		calls++
		if index >= len(responses) {
			t.Error("unexpected extra stage request")
			http.Error(w, "unexpected stage", 500)
			return
		}
		if check != nil {
			check(index, request.Messages)
		}
		if protocol == ProtocolOllama {
			writeOllamaPlan(w, responses[index])
		} else {
			writeOpenAIPlan(w, responses[index])
		}
	}))
	t.Cleanup(server.Close)
	connector, err := New(Options{Protocol: protocol, BaseURL: server.URL, Model: "arbitrary-model", StagedExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	return connector, &calls
}

func TestStagedExtractionSeparatesCandidateContextAcrossProviders(t *testing.T) {
	for _, protocol := range []string{ProtocolOllama, ProtocolOpenAI} {
		t.Run(protocol, func(t *testing.T) {
			c, calls := stagedFixture(t, protocol, []string{
				`{"facts":[{"subject":"Atlas","predicate":"uses","object":"Redis"}]}`,
				`{"type":"fact","negated":false}`,
				`{"entities":[{"name":"Atlas","type":"project","evidence_id":0},{"name":"Redis","type":"database","evidence_id":0}]}`,
				`{"relations":[{"fact":0,"candidate":0,"relation":"supports"}]}`,
			}, func(index int, messages []map[string]string) {
				source := messages[len(messages)-1]["content"]
				if index < 3 && strings.Contains(source, "candidate-only-marker") {
					t.Error("candidate leaked into extraction stage")
				}
				if index == 3 && !strings.Contains(source, "candidate-only-marker") {
					t.Error("relations lost candidate context")
				}
			})
			input := domain.EnrichmentInput{Text: "Atlas uses Redis.", Candidates: []memory.Memory{{Text: "candidate-only-marker"}}}
			out, err := c.Enrich(context.Background(), input)
			if err != nil || *calls != 4 || len(out.Facts) != 1 || out.Facts[0].Subject != "Atlas" || out.Facts[0].Predicate != "uses" || out.Facts[0].Object != "Redis" || len(out.Entities) != 2 || len(out.Relations) != 1 {
				t.Fatalf("staged output: %+v calls=%d err=%v", out, *calls, err)
			}
			if err := domain.ValidateEnrichment(input, out); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStagedExtractionRepairsOnlyInvalidStage(t *testing.T) {
	c, calls := stagedFixture(t, ProtocolOllama, []string{
		`{"facts":[{"subject":"Atlas","predicate":"uses","object":"Redis"}]}`,
		`{"type":"fact","negated":false}`,
		`{"entities":[{"name":"MongoDB","type":"database","evidence_id":0}]}`,
		`{"entities":[{"name":"Redis","type":"database","evidence_id":0}]}`,
	}, nil)
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	if err != nil || *calls != 4 || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "entities stage") || len(out.Entities) != 1 {
		t.Fatalf("bounded stage repair: %+v calls=%d err=%v", out, *calls, err)
	}
}

func TestStagedExtractionRejectsInvalidOutputWithoutFallback(t *testing.T) {
	for name, invalid := range map[string]string{
		"invented-object":    `{"facts":[{"subject":"Atlas","predicate":"uses","object":"MongoDB"}]}`,
		"invented-predicate": `{"facts":[{"subject":"Atlas","predicate":"dislikes","object":"Redis"}]}`,
		"wrong-span":         `{"facts":[{"subject":"Atlas","predicate":"uses","object":"Redis","evidence_id":1}]}`,
		"unknown-field":      `{"facts":[{"subject":"Atlas","predicate":"uses","object":"Redis","type":"fact"}]}`,
		"null-array":         `{"facts":null}`,
		"trailing-object":    `{"facts":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, calls := stagedFixture(t, ProtocolOllama, []string{invalid, invalid}, nil)
			_, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
			if !errors.Is(err, memory.ErrInvalid) || *calls != 2 {
				t.Fatalf("invalid output accepted or retry unbounded: calls=%d %v", *calls, err)
			}
		})
	}
}

func TestStagedClassificationCannotRewriteClaimsOrDefaultPolarity(t *testing.T) {
	for name, invalid := range map[string]string{
		"rewritten-claim":  `{"type":"fact","negated":false,"subject":"Redis"}`,
		"missing-polarity": `{"type":"fact"}`,
		"unsupported-type": `{"type":"unknown","negated":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, calls := stagedFixture(t, ProtocolOllama, []string{
				`{"facts":[{"subject":"Atlas","predicate":"uses","object":"Redis"}]}`, invalid, invalid,
			}, nil)
			_, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
			if !errors.Is(err, memory.ErrInvalid) || *calls != 3 {
				t.Fatalf("unsafe classification accepted: calls=%d %v", *calls, err)
			}
		})
	}
}

func TestStagedEntitiesHonorCustomProfileAndSourceSpan(t *testing.T) {
	profile := domain.DefaultExtractionProfile()
	profile.EntityTypes = []string{"person"}
	profile.FactTypes = []memory.EpistemicType{memory.EpistemicObservation}
	profile.EntityAttributes = map[string]domain.AttributeSchema{"role": {Type: "string"}}
	c, calls := stagedFixture(t, ProtocolOpenAI, []string{
		`{"facts":[{"subject":"Alice","predicate":"owns","object":"Atlas","evidence_id":1}]}`,
		`{"type":"observation","negated":false}`,
		`{"entities":[{"name":"Alice","type":"person","evidence_id":1,"attributes":{"role":"owner"}}]}`,
	}, func(index int, messages []map[string]string) {
		if index == 1 {
			for _, message := range messages {
				if message["role"] == "assistant" && (strings.Contains(message["content"], `"type":"fact"`) || strings.Contains(message["content"], `"type":"preference"`)) {
					t.Error("classification example violates tenant profile")
				}
			}
		}
	})
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Background only.\nAlice owns Atlas.", Profile: &profile})
	if err != nil || *calls != 3 || len(out.Entities) != 1 || out.Entities[0].Quote != "Alice owns Atlas." || out.Entities[0].Attributes["role"] != "owner" {
		t.Fatalf("custom profile or evidence lost: %+v %v", out, err)
	}
}

func TestStagedExtractionNoClaimsAndNoCandidatesSkipsUnneededStages(t *testing.T) {
	c, calls := stagedFixture(t, ProtocolOllama, []string{`{"facts":[]}`, `{"entities":[]}`}, nil)
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Good morning!"})
	if err != nil || *calls != 2 || len(out.Facts) != 0 || out.Entities == nil || out.Relations == nil {
		t.Fatalf("empty extraction: %+v calls=%d %v", out, *calls, err)
	}
}

func TestStagedExtractionRetriesTruncationWithBoundedBudget(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Options map[string]int `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		calls++
		if calls == 1 {
			if request.Options["num_predict"] != 512 {
				t.Error("initial budget changed")
			}
			json.NewEncoder(w).Encode(map[string]any{"done": true, "done_reason": "length", "message": map[string]string{"content": `{"facts":[`}})
			return
		}
		if request.Options["num_predict"] != 1024 {
			t.Error("truncation retry did not double bounded budget")
		}
		writeOllamaPlan(w, `{"facts":[]}`)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "test", NumPredict: 512, StagedExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Facts []stagedClaim `json:"facts"`
	}
	var notes []string
	err = extractionStage(c, context.Background(), "claims", "extract claims", "Hello", nil, &result, func() error {
		if result.Facts == nil {
			return errors.New("missing facts")
		}
		return nil
	}, &notes)
	if err != nil || calls != 2 || len(notes) != 1 || c.options.NumPredict != 512 {
		t.Fatalf("truncation repair: calls=%d notes=%v error=%v", calls, notes, err)
	}
}

func TestStagedExtractionDoesNotRetryTransportFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "test", StagedExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	if err == nil || errors.Is(err, memory.ErrInvalid) || calls != 1 {
		t.Fatalf("transport failure hidden or retried: calls=%d err=%v", calls, err)
	}
}

func TestStagedEntityEvidence(t *testing.T) {
	var response struct {
		Entities []evidenceEntity `json:"entities"`
	}
	if err := json.Unmarshal([]byte(`{"entities":[{"name":"Atlas","type":"person","evidence_id":0},{"name":"Redis","type":"service","evidence_id":0}]}`), &response); err != nil {
		t.Fatal(err)
	}
	out, err := (evidenceResponse{Enrichment: domain.Enrichment{Facts: []domain.ExtractedFact{}, Relations: []domain.ExtractedRelation{}}, Entities: response.Entities}).resolve(sourceEvidence("Atlas uses Redis."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.NormalizeEnrichment(domain.EnrichmentInput{Text: "Atlas uses Redis."}, out); err != nil {
		t.Fatalf("%+v %v", out, err)
	}
}
