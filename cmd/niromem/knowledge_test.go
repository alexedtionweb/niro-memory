package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

type deadlineTransport struct {
	t       *testing.T
	minimum time.Duration
}

func (d deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if d.minimum > 0 {
		var body struct {
			Think *bool `json:"think"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			d.t.Error(err)
		}
		if body.Think == nil || *body.Think {
			d.t.Error("thinking was not explicitly disabled")
		}
	}
	deadline, ok := r.Context().Deadline()
	minimum := d.minimum
	if minimum == 0 {
		minimum = 9 * time.Minute
	}
	if !ok || time.Until(deadline) < minimum {
		d.t.Error("provider HTTP client shortened the inference budget")
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":{"content":"{\"operations\":[{\"type\":\"ADD\",\"reason_code\":\"explicit_add\"}]}"}}`))}, nil
}

func TestCPUKnowledgeDeadlineSurvivesHTTPAndWorkerCaps(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = deadlineTransport{t: t}
	defer func() { http.DefaultTransport = old }()
	p, err := knowledgePlanner(Choice{Use: "ollama", Options: map[string]json.RawMessage{"ollama": json.RawMessage(`{"base_url":"http://fixture.invalid","model":"fixture"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := p.Plan(ctx, knowledge.Input{OperationKey: "deadline", Text: "Atlas uses Redis."}, knowledge.State{}); err != nil {
		t.Fatal(err)
	}
	e, err := niromem.Open(niromem.Options{Store: memstore.New(nil), Knowledge: niromem.KnowledgeOptions{Timeout: 10 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Worker().Timeout < 31*time.Minute {
		t.Fatal("worker deadline can truncate multiple permitted inference calls")
	}
}

func TestKnowledgeAdmissionConnectorConfiguration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := &Config{
			Store: Choice{
				Use: "sqlite",
				Options: map[string]json.RawMessage{
					"sqlite": json.RawMessage(`{"path":":memory:"}`),
				},
			},
		}

		cfg.Knowledge.Admission.Enabled = enabled
		cfg.Knowledge.Admission.MaxTextBytes = 17
		var options niromem.Options
		closers, err := connectors(context.Background(), cfg, &options)
		if err != nil {
			t.Fatal(err)
		}
		for _, closer := range closers {
			defer closer.Close()
		}

		if !enabled {
			if options.Knowledge.Admission != nil {
				t.Fatal("admission must be disabled by default")
			}
			continue
		}
		decider, ok := options.Knowledge.Admission.(knowledge.AdmissionDeterministic)
		if !ok || decider.MaxTextBytes != 17 {
			t.Fatalf("admission connector: %#v", options.Knowledge.Admission)
		}
	}
	cfg := &Config{}
	if err := cfg.validateKnowledge(); err != nil || cfg.Knowledge.Admission.Enabled ||
		cfg.Knowledge.Admission.MaxTextBytes != 32768 {
		t.Fatalf("admission defaults: %+v %v", cfg.Knowledge.Admission, err)
	}
}

func TestRuntimeKnowledgeProfiles(t *testing.T) {
	for _, runtime := range []bool{false, true} {
		storeOptions, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "runtime.sqlite")})
		if err != nil {
			t.Fatal(err)
		}

		cfg := &Config{knowledgeRuntime: runtime, Store: Choice{Use: "sqlite",
			Options: map[string]json.RawMessage{"sqlite": storeOptions}}}
		cfg.Knowledge.Profiles = map[string]Choice{"local": {Use: "ollama", Options: map[string]json.RawMessage{
			"ollama": json.RawMessage(`{"base_url":"http://localhost:11434","model":"tiny-model","api_key":"never-expose"}`),
		}}}
		var options niromem.Options
		closers, err := connectors(context.Background(), cfg, &options)
		if err != nil {
			t.Fatal(err)
		}
		for _, closer := range closers {
			defer closer.Close()
		}
		if !runtime {
			if len(options.KnowledgePlanners) != 0 {
				t.Fatal("runtime profiles enabled outside dev UI")
			}
			continue
		}
		engine, err := niromem.Open(options)
		if err != nil {
			t.Fatal(err)
		}
		report, err := engine.KnowledgeConfiguration(context.Background(), "tenant")
		engine.Close()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if report.Profiles["local"].Model != "tiny-model" || report.Profiles["local"].Provider != "ollama" {
			t.Fatalf("missing provider diagnostics: %s", raw)
		}
		if strings.Contains(string(raw), "never-expose") || strings.Contains(string(raw), "localhost") {
			t.Fatal("provider configuration secrets exposed")
		}
	}
}

func TestQwen3ConfigurationEnablesLivePlannerAndSemanticEmbedder(t *testing.T) {
	cfg, err := loadConfig("../../configs/qwen3.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worker.Concurrency != 1 || cfg.Knowledge.TimeoutMS != 600000 {
		t.Fatal("CPU example must serialize background inference and allow cold starts")
	}
	storeOptions, err := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "qwen3.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store.Options = map[string]json.RawMessage{"sqlite": storeOptions}
	var options niromem.Options
	closers, err := connectors(context.Background(), cfg, &options)
	if err != nil {
		t.Fatal(err)
	}
	for _, closer := range closers {
		defer closer.Close()
	}
	if options.Knowledge.Mode != "enforced" || options.Embedder == nil || !options.Knowledge.BackgroundEnrichment {
		t.Fatal("example must enable confirmed saves and semantic embeddings")
	}
	model := options.Embedder.Model()
	if model.ID != "ollama/nomic-embed-text@1" || model.Dims != 768 {
		t.Fatalf("embedding space: %+v", model)
	}
	engine, err := niromem.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	report, err := engine.KnowledgeConfiguration(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if report.Effective.Model != "qwen3:0.6b" || report.Effective.Provider != "ollama" || !report.Effective.BackgroundEnrichment {
		t.Fatalf("decision planner: %+v", report.Effective)
	}
}

func TestKnowledgePlannerDecodesCPULimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Options map[string]int `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Options["num_thread"] != 2 || body.Options["num_ctx"] != 3072 || body.Options["num_predict"] != 640 {
			t.Errorf("configured generation limits lost: %+v", body.Options)
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"operations":[{"type":"ADD","reason_code":"explicit_add"}]}`}})
	}))
	defer server.Close()
	raw, err := json.Marshal(map[string]any{"base_url": server.URL, "model": "fixture",
		"num_thread": 2, "num_ctx": 3072, "num_predict": 640, "think": false})
	if err != nil {
		t.Fatal(err)
	}
	p, err := knowledgePlanner(Choice{Use: "ollama", Options: map[string]json.RawMessage{"ollama": raw}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.Plan(ctx, knowledge.Input{OperationKey: "test", Text: "Atlas uses Redis."}, knowledge.State{}); err != nil {
		t.Fatal(err)
	}
}
