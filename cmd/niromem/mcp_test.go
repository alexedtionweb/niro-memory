package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/devui"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestMCPAuthAndGuards(t *testing.T) {
	e, err := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, address := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		if _, err := mcpHandler(e, api.Dev{}, address, "project", "claude-code"); err == nil {
			t.Fatalf("public MCP allowed at %s", address)
		}
	}
	if _, err := mcpHandler(e, api.Dev{}, "localhost:8080", "", "claude-code"); err == nil {
		t.Fatal("empty MCP user allowed")
	}
	const (
		key       = "mcp-bound-key-012345"
		otherKey  = "other-project-key-012345"
		tenantKey = "other-tenant-key-012345"
	)
	auth, err := api.NewKeys([]api.Key{
		{Key: key, Tenant: "one", User: "project", Agent: "claude-code"},
		{Key: otherKey, Tenant: "one", User: "other", Agent: "claude-code"},
		{Key: tenantKey, Tenant: "two", User: "project", Agent: "claude-code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcpHandler(e, auth, "localhost:8080", "project", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		code int
	}{{"", 401}, {otherKey, 403}, {key, 200}} {
		r := httptest.NewRequest("POST", "http://localhost:8080/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.Header.Set("Authorization", "Bearer "+tc.key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("got %d, want %d: %s", w.Code, tc.code, w.Body.String())
		}
	}
	call := func(key, body string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "http://localhost:8080/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("tool failed: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	call(key, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"Tenant one connector convention"}}}`)
	query := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"connector convention"}}}`
	if !strings.Contains(call(key, query), "Tenant one connector convention") {
		t.Fatal("tenant cannot retrieve its own memory")
	}
	if strings.Contains(call(tenantKey, query), "Tenant one connector convention") {
		t.Fatal("MCP leaked memory across tenants")
	}
}

func TestServeAllInOneAndPersistence(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "server.json")
	raw, err := json.Marshal(map[string]any{
		"env": "development", "listen": address,
		"store": map[string]any{"use": "sqlite", "sqlite": map[string]any{"path": filepath.Join(dir, "memory.db")}},
		"auth":  map[string]any{"dev": true}, "worker": map[string]any{"inline": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIROMEM_ENV", "development")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpClient := &http.Client{Timeout: time.Second}
	baseURL := "http://" + address
	start := func() func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		args := []string{"-config", cfg, "-mcp", "-mcp-user", "project"}
		if devui.Enabled {
			args = append(args, "-dev-ui")
		}
		go func() { done <- serve(ctx, log, args) }()
		stop := func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("serve did not shut down")
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			res, err := httpClient.Get(baseURL + "/healthz")
			if err == nil {
				body, readErr := io.ReadAll(res.Body)
				res.Body.Close()
				if readErr == nil && res.StatusCode == 200 && string(body) == "ok" {
					return stop
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		stop()
		t.Fatal("API did not become ready")
		return nil
	}
	call := func(body string) string {
		t.Helper()
		req, err := http.NewRequest("POST", baseURL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("MCP response: %s %v", data, err)
		}
		var result struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &result); err != nil || result.Result.IsError || len(result.Error) != 0 {
			t.Fatalf("MCP tool failed: %s %v", data, err)
		}
		return string(data)
	}
	stop := start()
	func() {
		defer stop()
		if devui.Enabled {
			res, err := httpClient.Get(baseURL + "/_dev/")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("UI returned %d", res.StatusCode)
			}
		}
		call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
		call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"Validate connector input before writes.","slot":"convention.input_validation"}}}`)
	}()
	stop = start()
	defer stop()
	result := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"connector input validation"}}}`)
	if !strings.Contains(result, "Validate connector input before writes.") {
		t.Fatalf("memory not persisted across restart: %s", result)
	}
}
