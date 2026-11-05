package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDevConsoleGuards(t *testing.T) {
	cfg := &Config{Env: "development", Listen: "127.0.0.1:8080"}
	if err := devAllowed(cfg); err == nil {
		t.Skip("built with -tags devui; guard below covers production")
	}
	cfg.Listen = "0.0.0.0:8080"
	if devAllowed(cfg) == nil {
		t.Fatal("public address allowed")
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	t.Setenv("PG_DSN", "postgres://x")
	cfg, err := loadConfig(write(`{"env":"production","store":{"use":"postgres","postgres":{"dsn":"${PG_DSN}"}},"auth":{"keys":[{"sha256":"ab","tenant":"t"}]}}`))
	if err != nil || cfg.Store.Use != "postgres" || string(cfg.Store.Options["postgres"]) != `{"dsn":"postgres://x"}` {
		t.Fatalf("%+v %v", cfg, err)
	}
	if cfg.Knowledge.Mode != "disabled" || cfg.Knowledge.Planner.Use != "deterministic" ||
		cfg.Knowledge.MaxCandidates != 50 || cfg.Knowledge.MaxGraphHops != 3 || cfg.Knowledge.TimeoutMS != 2000 {
		t.Fatalf("knowledge defaults: %+v", cfg.Knowledge)
	}
	if _, err := loadConfig(write(`{"env":"production","auth":{"dev":true}}`)); err == nil {
		t.Fatal("dev auth allowed in production")
	}
	if _, err := loadConfig(write(`{"env":"development"}`)); err == nil {
		t.Fatal("no auth configured must fail")
	}
	if _, err := loadConfig(write(`{"env":"development","auth":{"dev":true},"typo":1}`)); err == nil {
		t.Fatal("unknown keys must fail")
	}
	for _, invalid := range []string{
		`{"auth":{"dev":true},"knowledge":{"mode":"automatic"}}`,
		`{"auth":{"dev":true},"knowledge":{"max_candidates":1001}}`,
		`{"auth":{"dev":true},"knowledge":{"max_graph_hops":9}}`,
		`{"auth":{"dev":true},"knowledge":{"timeout_ms":1800001}}`,
		`{"auth":{"dev":true},"knowledge":{"planner":{"use":"unknown"}}}`,
		`{"auth":{"dev":true},"knowledge":{"admission":{"enabled":true,"max_text_bytes":-1}}}`,
		`{"auth":{"dev":true},"knowledge":{"admission":{"enabled":true,"max_text_bytes":1048577}}}`,
	} {
		if _, err := loadConfig(write(invalid)); err == nil {
			t.Fatalf("invalid knowledge config accepted: %s", invalid)
		}
	}
	cfg, err = loadConfig(write(`{"auth":{"dev":true},"knowledge":{"mode":"shadow","planner":{"use":"deterministic"},"max_candidates":17,"max_graph_hops":4,"timeout_ms":500,"admission":{"enabled":true,"max_text_bytes":1048576}}}`))
	if err != nil || cfg.Knowledge.Mode != "shadow" || cfg.Knowledge.MaxCandidates != 17 ||
		cfg.Knowledge.MaxGraphHops != 4 || cfg.Knowledge.TimeoutMS != 500 {
		t.Fatalf("valid knowledge config: %+v %v", cfg, err)
	}
	if !cfg.Knowledge.Admission.Enabled || cfg.Knowledge.Admission.MaxTextBytes != 1<<20 {
		t.Fatalf("admission config: %+v", cfg.Knowledge.Admission)
	}
}

func TestKnowledgeConfigExample(t *testing.T) {
	cfg, err := loadConfig("../../configs/knowledge.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Knowledge.Mode != "disabled" || cfg.Knowledge.Planner.Use != "deterministic" {
		t.Fatalf("example must be disabled and deterministic: %+v", cfg.Knowledge)
	}
}

func TestDevConsoleRefusedInProduction(t *testing.T) {
	cfg := &Config{Env: "production", Listen: "127.0.0.1:8080"}
	if devAllowed(cfg) == nil {
		t.Fatal("console allowed in production")
	}
	t.Setenv("NIROMEM_ENV", "production")
	if devAllowed(&Config{Env: "development", Listen: "127.0.0.1:8080"}) == nil {
		t.Fatal("console allowed with NIROMEM_ENV=production")
	}
}
