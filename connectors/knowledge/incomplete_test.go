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

func TestTruncatedExtractionRetriesWithLargerBudgetWithoutInventingJSON(t *testing.T) {
	for _, reportedLimit := range []bool{false, true} {
		t.Run(map[bool]string{false: "unexpected-eof", true: "token-limit"}[reportedLimit], func(t *testing.T) {
			var budgets []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Options struct {
						NumPredict int `json:"num_predict"`
					} `json:"options"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				budgets = append(budgets, request.Options.NumPredict)
				if len(budgets) == 1 {
					reason := "stop"
					if reportedLimit {
						reason = "length"
					}
					json.NewEncoder(w).Encode(map[string]any{"done": true, "done_reason": reason, "message": map[string]string{"content": `{"facts":[`}})
					return
				}
				writeOllamaPlan(w, `{"facts":[{"quote":"Atlas uses Redis.","subject":"Atlas","subject_type":"project","predicate":"uses","object":"Redis","object_type":"database","type":"fact","negated":false}],"entities":[],"relations":[]}`)
			}))
			defer server.Close()
			c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture", NumPredict: 768})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
			if err != nil || len(out.Facts) != 1 || len(out.Notes) != 1 {
				t.Fatalf("retry failed %+v %v", out, err)
			}
			if len(budgets) != 2 || budgets[0] != 768 || budgets[1] != 1536 {
				t.Fatalf("unbounded or unchanged budgets %v", budgets)
			}
		})
	}
}

func TestRepeatedTruncationIsRetryableNotInvalidEvidence(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeOllamaPlan(w, `{"facts":[`)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture", NumPredict: 2048})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	var incomplete interface{ Temporary() bool }
	if err == nil || errors.Is(err, memory.ErrInvalid) || !errors.As(err, &incomplete) || !incomplete.Temporary() || requests != 2 {
		t.Fatalf("truncation incorrectly permanent, requests=%d error=%v", requests, err)
	}
}

func TestOutputBudgetRetryStopsAtConfiguredCeiling(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeOllamaPlan(w, `{"facts":[`)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "fixture", NumPredict: 4096})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	if err == nil || errors.Is(err, memory.ErrInvalid) || requests != 1 || c.options.NumPredict != 4096 {
		t.Fatalf("ceiling exceeded or truncation permanently rejected: requests=%d error=%v", requests, err)
	}
}

func TestProviderFinishReasonRejectsTruncationEvenWithValidPartialJSON(t *testing.T) {
	for _, protocol := range []string{ProtocolOllama, ProtocolOpenAI} {
		c, err := New(Options{Protocol: protocol, BaseURL: "http://localhost", Model: "fixture"})
		if err != nil {
			t.Fatal(err)
		}

		body := `{"done":true,"done_reason":"length","message":{"content":"{}"}}`
		if protocol == ProtocolOpenAI {
			body = `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`
		}
		_, err = c.responseContent([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "incomplete model output") {
			t.Fatalf("accepted partial %s: %v", protocol, err)
		}
	}
}
