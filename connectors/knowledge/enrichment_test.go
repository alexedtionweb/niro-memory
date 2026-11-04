package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
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
		writeOpenAIPlan(w, `{"facts":[{"quote":"Atlas uses Redis.","subject":"Atlas","predicate":"uses","object":"Redis","type":"fact","negated":false}],"relations":[]}`)
	}))
	defer server.Close()
	c, err := New(Options{Protocol: ProtocolOpenAI, BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
	if err != nil || len(out.Facts) != 1 || out.Model != "fixture" {
		t.Fatalf("compatible extraction failed: %+v %v", out, err)
	}
}

func TestCPUEnrichmentProtocolAndGrounding(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "grounded", true: "fabricated"}[invalid], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				if len(body.Messages) != 10 {
					t.Error("missing extraction prompt")
				}
				quote := "Atlas uses Redis."
				if invalid {
					quote = "Atlas uses MySQL."
				}
				content, _ := json.Marshal(map[string]any{"facts": []any{map[string]string{"quote": quote, "subject": "Atlas", "predicate": "uses", "object": "Redis", "type": "fact"}}, "relations": []any{}})
				json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(content)}})
			}))
			defer server.Close()
			c, err := New(Options{Protocol: "ollama", BaseURL: server.URL, Model: "cpu-test"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Enrich(context.Background(), domain.EnrichmentInput{Text: "Atlas uses Redis."})
			if invalid && err == nil {
				t.Fatal("invented evidence accepted")
			}
			if !invalid && (err != nil || len(out.Facts) != 1 || out.Model != "cpu-test") {
				t.Fatalf("extraction: %+v %v", out, err)
			}
		})
	}
}
