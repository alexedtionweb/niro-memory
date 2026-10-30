package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestHTTPValidation(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name, method, host, origin, contentType, accept, version, body string
		status                                                         int
	}{
		{"ping", "POST", "localhost:3001", "", "application/json", "application/json, text/event-stream", protocolVersion, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, 200},
		{"same-origin", "POST", "127.0.0.1:3001", "http://127.0.0.1:3001", "application/json", "application/json", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, 200},
		{"notification", "POST", "localhost:3001", "", "application/json", "application/json", "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202},
		{"rebind", "POST", "evil.example:3001", "", "application/json", "application/json", "", `{}`, 403},
		{"wrong-port", "POST", "localhost:3002", "", "application/json", "application/json", "", `{}`, 403},
		{"foreign-origin", "POST", "localhost:3001", "https://evil.example", "application/json", "application/json", "", `{}`, 403},
		{"other-port-origin", "POST", "localhost:3001", "http://localhost:3002", "application/json", "application/json", "", `{}`, 403},
		{"null-origin", "POST", "localhost:3001", "null", "application/json", "application/json", "", `{}`, 403},
		{"get", "GET", "localhost:3001", "", "", "text/event-stream", "", "", 405},
		{"delete", "DELETE", "localhost:3001", "", "", "", "", "", 405},
		{"wrong-content-type", "POST", "localhost:3001", "", "text/plain", "application/json", "", `{}`, 415},
		{"wrong-accept", "POST", "localhost:3001", "", "application/json", "text/event-stream", "", `{}`, 406},
		{"version", "POST", "localhost:3001", "", "application/json", "application/json", "unknown", `{}`, 400},
		{"bad-json", "POST", "localhost:3001", "", "application/json", "application/json", "", `{`, 400},
		{"batch", "POST", "localhost:3001", "", "application/json", "application/json", "", `[]`, 400},
		{"large-body", "POST", "localhost:3001", "", "application/json", "application/json", "", strings.Repeat("x", (1<<20)+1), 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://"+tc.host+"/mcp", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Accept", tc.accept)
			req.Header.Set("MCP-Protocol-Version", tc.version)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			s.HTTPHandler("3001", "").ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == 202 && rec.Body.Len() != 0 {
				t.Fatal("notification returned a body")
			}
			if tc.status == 405 && rec.Header().Get("Allow") != "POST" {
				t.Fatal("missing Allow header")
			}
		})
	}
}

func TestHTTPToolsEndToEnd(t *testing.T) {
	engine, err := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const backendKey = "backend-mcp-test-key-123"
	auth, err := api.NewKeys([]api.Key{{Key: backendKey, Tenant: "dev", User: "project", Agent: "claude-code"}})
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer((&api.Server{Engine: engine, Auth: auth}).Handler())
	defer backend.Close()
	s := &Server{client: client.New(backend.URL, backendKey), user: "project", agent: "claude-code"}
	mcp := httptest.NewUnstartedServer(nil)
	_, port, err := net.SplitHostPort(mcp.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mcp.Config.Handler = s.HTTPHandler(port, "mcp-test-token")
	mcp.Start()
	defer mcp.Close()
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"remember_memory","arguments":{"text":"Use adapters for storage providers."}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"storage providers"}}}`,
	} {
		req, err := http.NewRequestWithContext(context.Background(), "POST", mcp.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer mcp-test-token")
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
		res, err := mcp.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "notifications/initialized") {
			if res.StatusCode != 202 || len(data) != 0 {
				t.Fatalf("notification: %d %s", res.StatusCode, data)
			}
			continue
		}
		var decoded response
		if res.StatusCode != 200 || json.Unmarshal(data, &decoded) != nil || decoded.Error != nil {
			t.Fatalf("HTTP MCP failure: %d %s", res.StatusCode, data)
		}
		if strings.Contains(body, "tools/call") && decoded.Result.(map[string]any)["isError"] != false {
			t.Fatalf("tool failure: %s", data)
		}
		if strings.Contains(body, "search_memory") && !strings.Contains(string(data), "Use adapters for storage providers.") {
			t.Fatalf("missing saved memory: %s", data)
		}
	}
	for _, token := range []string{"", "wrong-token"} {
		req := httptest.NewRequest("POST", "http://localhost:"+port+"/mcp", nil)
		req.Header.Set("Authorization", token)
		rec := httptest.NewRecorder()
		s.HTTPHandler(port, "mcp-test-token").ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("bad token accepted: %d", rec.Code)
		}
	}
}
