package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

func TestOpenAIEnrichmentUsesExistingCompatibleProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("compatible endpoint/auth not preserved")
		}

		var body struct {
			ResponseFormat map[string]string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ResponseFormat["type"] != "json_object" {
			t.Error("missing structured response request")
		}
		writeOpenAIPlan(w, `{"facts":[{"quote":"Atlas uses Redis.","subject":"Atlas","predicate":"uses","object":"Redis","type":"fact","negated":false}],"entities":[{"name":"Atlas","type":"project","quote":"Atlas uses Redis."},{"name":"Redis","type":"database","quote":"Atlas uses Redis."}],"relations":[]}`)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOpenAI, BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	if err != nil || len(out.Facts) != 1 || len(out.Entities) != 2 || out.Model != "fixture" {
		t.Fatalf("compatible extraction failed: %+v %v", out, err)
	}
}

func TestTenantExtractionSchemaReachesProvider(t *testing.T) {
	p := domain.DefaultExtractionProfile()
	p.EntityTypes = []string{"person"}
	p.FactTypes = nil
	// Entity-only extraction still uses a valid bounded fact vocabulary.
	p.FactTypes = domain.DefaultExtractionProfile().FactTypes[:1]
	p.EntityAttributes = map[string]domain.AttributeSchema{"role": {Type: "string", Enum: []string{"owner"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Format   json.RawMessage     `json:"format"`
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		schema, err := domain.EvidenceExtractionJSONSchema(&p, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		json.Unmarshal(body.Format, &got)
		json.Unmarshal(schema, &want)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("provider did not receive tenant schema: %s", body.Format)
		}
		for _, message := range body.Messages {
			if message["role"] == "assistant" && strings.Contains(message["content"], `"type":"project"`) {
				t.Error("example violates configured entity vocabulary")
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"facts":[],"entities":[{"name":"Alice","type":"person","quote":"Alice owns Atlas.","attributes":{"role":"owner"}}],"relations":[]}`}})
	}))
	defer server.Close()
	c, err := New(Options{Protocol: "ollama", BaseURL: server.URL, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Alice owns Atlas.", Profile: &p})
	if err != nil || len(out.Entities) != 1 || out.Entities[0].Attributes["role"] != "owner" {
		t.Fatalf("typed entity extraction: %+v %v", out, err)
	}
}

func TestCPUEnrichmentProtocolAndGrounding(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "grounded", true: "fabricated"}[invalid], func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body struct {
					Think    bool                `json:"think"`
					Format   map[string]any      `json:"format"`
					Options  map[string]int      `json:"options"`
					Messages []map[string]string `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/api/chat" || body.Think || body.Options["num_gpu"] != 0 || body.Options["num_ctx"] != 4096 || body.Format["type"] != "object" {
					t.Errorf("CPU schema contract: %+v", body)
				}
				if len(body.Messages) != 12 {
					t.Fatal("expected compact extraction prompt")
				}
				prompt := body.Messages[len(body.Messages)-1]["content"]
				context := body.Messages[len(body.Messages)-3]["content"]
				var contextFields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(context), &contextFields); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(prompt, `"text":"Atlas uses Redis."`) || strings.Contains(prompt, `"candidates":`) ||
					(requests == 1 && !strings.Contains(context, "Other uses MySQL.")) || contextFields["text"] != nil {
					t.Error("source text and candidate context must use separate messages")
				}
				if !strings.Contains(body.Messages[1]["content"], "Fabrikam uses Redis.") ||
					strings.Contains(body.Messages[2]["content"], "Fabrikam") {
					t.Error("missing example that excludes candidate-only claims")
				}
				if !strings.Contains(body.Messages[len(body.Messages)-4]["content"], `"object":"CHÁ GELADO"`) ||
					!strings.Contains(body.Messages[len(body.Messages)-4]["content"], `"type":"preference"`) {
					t.Error("missing Portuguese multiword preference example")
				}
				quote := "Atlas uses Redis."
				if invalid {
					quote = "Atlas uses MySQL."
				}
				content, _ := json.Marshal(map[string]any{"facts": []any{map[string]string{"quote": quote, "subject": "Atlas", "predicate": "uses", "object": "Redis", "type": "fact"}}, "entities": []any{}, "relations": []any{}})
				json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(content)}})
			}))
			defer server.Close()
			c, err := New(Options{Protocol: "ollama", BaseURL: server.URL, Model: "cpu-test"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis.",
				Candidates: []memory.Memory{{Text: "Other uses MySQL."}}})
			if invalid && err == nil {
				t.Fatal("invented evidence accepted")
			}
			if requests > 2 || invalid && requests != 2 {
				t.Fatalf("unbounded extraction retries: %d", requests)
			}
			if !invalid && (err != nil || len(out.Facts) != 1 || out.Model != "cpu-test") {
				t.Fatalf("extraction: %+v %v", out, err)
			}
		})
	}
}
