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
	if _, err := loadConfig(write(`{"env":"production","auth":{"dev":true}}`)); err == nil {
		t.Fatal("dev auth allowed in production")
	}
	if _, err := loadConfig(write(`{"env":"development"}`)); err == nil {
		t.Fatal("no auth configured must fail")
	}
	if _, err := loadConfig(write(`{"env":"development","auth":{"dev":true},"typo":1}`)); err == nil {
		t.Fatal("unknown keys must fail")
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
