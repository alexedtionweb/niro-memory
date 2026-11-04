package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/knowledge"
)

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
