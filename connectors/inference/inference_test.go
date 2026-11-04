package inference_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexedtionweb/niro-memory/connectors/inference"
	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/relations"
)

func TestProtocols(t *testing.T) {
	for _, protocol := range []string{"openai", "ollama"} {
		t.Run(protocol, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/chat/completions"
				if protocol == "ollama" {
					want = "/api/chat"
				}
				if r.URL.Path != want {
					t.Errorf("wrong protocol path %s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["stream"] != false || body["model"] != "fixture" {
					t.Errorf("unexpected body %+v", body)
				}
				content := `{"links":[{"target_id":"b","relation":"supports","reason":"Related tests.","confidence":0.8}]}`
				if protocol == "openai" {
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
				} else {
					json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": content}})
				}
			}))
			defer srv.Close()
			p, err := inference.New(inference.Options{Protocol: protocol, BaseURL: srv.URL, Model: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			props, err := p.Infer(context.Background(), relations.Input{Source: memory.Memory{ID: "a", Text: "test"}, Candidates: []memory.Memory{{ID: "b", Text: "tests"}}})
			if err != nil || len(props) != 1 || props[0].Target != "b" {
				t.Fatalf("proposals %+v %v", props, err)
			}
		})
	}
}

func TestMalformedInference(t *testing.T) {
	for _, content := range []string{`{}`, `not json`, `{"links":null}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": content}})
		}))
		p, err := inference.New(inference.Options{Protocol: "ollama", BaseURL: srv.URL, Model: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Infer(context.Background(), relations.Input{}); err == nil {
			t.Errorf("accepted malformed inference %s", content)
		}
		srv.Close()
	}
}
