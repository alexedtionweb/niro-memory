package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/memory"
)

func TestOpenAICompatiblePlannerRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization header missing")
		}
		var payload struct {
			Model          string `json:"model"`
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Model != "fixture-model" || payload.ResponseFormat.Type != "json_object" {
			t.Errorf("request payload = %#v", payload)
		}
		writeOpenAIPlan(w, `{"operations":[{"type":"ADD","reason_code":"new_knowledge"}]}`)
	}))
	defer server.Close()

	connector, err := New(Options{
		Protocol: ProtocolOpenAI, BaseURL: server.URL + "/v1",
		APIKey: "test-key", Model: "fixture-model", HTTP: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := connector.Plan(context.Background(), sampleInput(), domain.State{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Provider != "llm/"+ProtocolOpenAI || plan.Model != "fixture-model" ||
		len(plan.Operations) != 1 || plan.Operations[0].Type != domain.ADD {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Operations[0].Memory.Tenant != "tenant-1" || plan.Operations[0].Memory.Text != "User likes tea" {
		t.Fatalf("incoming memory was not preserved: %#v", plan.Operations[0].Memory)
	}
}

func TestOllamaNativeProtocolAndTargetValidation(t *testing.T) {
	target := memory.Memory{
		ID: "memory-1", Scope: memory.Scope{Tenant: "tenant-1", User: "user-1"},
		Kind: memory.Semantic, Layer: memory.Atom, Text: "fact", Version: 4,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var payload struct {
			Model  string `json:"model"`
			Format string `json:"format"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Model != "local-fixture" || payload.Format != "json" || payload.Stream {
			t.Errorf("request payload = %#v", payload)
		}
		writeOllamaPlan(w, `{"operations":[{"type":"CORRECT","target_id":"memory-1","expected_version":4,"reason_code":"source_correction"}]}`)
	}))
	defer server.Close()

	connector, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL, Model: "local-fixture", HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := connector.Plan(context.Background(), sampleInput(), domain.State{Current: []memory.Memory{target}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operations[0].TargetID != target.ID || plan.Operations[0].ExpectedVersion != target.Version {
		t.Fatalf("target binding = %#v", plan.Operations[0])
	}
}

func TestOllamaThinkingOption(t *testing.T) {
	for _, mode := range []string{"default", "disabled", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			var think *bool
			if mode != "default" {
				value := mode == "enabled"
				think = &value
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if think == nil {
					if _, exists := body["think"]; exists {
						t.Error("default model thinking must remain unchanged")
					}
				} else if string(body["think"]) != fmt.Sprint(*think) {
					t.Errorf("think = %s, want %v", body["think"], *think)
				}
				writeOllamaPlan(w, `{"operations":[{"type":"ADD","reason_code":"explicit_add"}]}`)
			}))
			defer server.Close()
			connector, err := New(Options{Protocol: ProtocolOllama, BaseURL: server.URL,
				Model: "qwen3:1.7b", Think: think, HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connector.Plan(context.Background(), sampleInput(), domain.State{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPlannerRejectsInvalidModelSchemasAndReferences(t *testing.T) {
	for _, response := range []string{
		`{"operations":[{"type":"DELETE","reason_code":"bad"}]}`,
		`{"operations":[{"type":"ADD","reason_code":"ok","unexpected":true}]}`,
		`{"operations":[{"type":"ADD","reason_code":"ok","confidence":1.5}]}`,
		`{"operations":[{"type":"ADD"}]}`,
		`{"operations":[{"type":"RETRACT","target_id":"outside","expected_version":1,"reason_code":"bad"}]}`,
		`{"operations":[{"type":"ADD","reason_code":"ok","relations":[{"source_id":"x","target_id":"y","relation":"made_up","reason_code":"x"}]}]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeOpenAIPlan(w, response)
		}))
		connector, err := New(Options{Protocol: ProtocolOpenAI, BaseURL: server.URL, Model: "fixture", HTTP: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		_, err = connector.Plan(context.Background(), sampleInput(), domain.State{})
		server.Close()
		if err == nil {
			t.Fatalf("response accepted: %s", response)
		}
	}
}

func TestModelADDWithOccupiedSlotIsRejectedByDomainValidator(t *testing.T) {
	old := memory.Memory{
		ID: "memory-1", Scope: memory.Scope{Tenant: "tenant-1", User: "user-1"},
		Kind: memory.Semantic, Layer: memory.Atom, Text: "User likes coffee",
		Slot: "user.preference", Version: 2, Visibility: memory.Team,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIPlan(w, `{"operations":[{"type":"ADD","reason_code":"new_knowledge"}]}`)
	}))
	defer server.Close()
	connector, err := New(Options{Protocol: ProtocolOpenAI, BaseURL: server.URL, Model: "fixture", HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput()
	input.Slot = old.Slot
	plan, err := connector.Plan(context.Background(), input, domain.State{Current: []memory.Memory{old}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = domain.Validate(input, plan, domain.State{Current: []memory.Memory{old}}, memory.Principal{User: "user-1"})
	var validation *domain.ValidationError
	if !errors.As(err, &validation) || validation.Code != "occupied_slot" {
		t.Fatalf("validation error = %v, want occupied_slot", err)
	}
}

func TestPlannerBoundsResponseAndRequiresExplicitConfiguration(t *testing.T) {
	if _, err := New(Options{Protocol: ProtocolOpenAI, BaseURL: "http://localhost"}); err == nil {
		t.Fatal("missing model was accepted")
	}
	if _, err := New(Options{Protocol: ProtocolOpenAI, Model: "fixture"}); err == nil {
		t.Fatal("missing base URL was accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseSize+1)))
	}))
	defer server.Close()
	connector, err := New(Options{Protocol: ProtocolOpenAI, BaseURL: server.URL, Model: "fixture", HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connector.Plan(context.Background(), sampleInput(), domain.State{}); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func writeOpenAIPlan(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
	})
}

func writeOllamaPlan(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message": map[string]string{"content": content},
	})
}

func sampleInput() domain.Input {
	return domain.Input{
		OperationKey: "op-1",
		Scope:        memory.Scope{Tenant: "tenant-1", User: "user-1"},
		Text:         "User likes tea",
		Trust:        memory.TrustNormal,
	}
}
