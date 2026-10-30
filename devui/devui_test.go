//go:build devui

package devui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/devui"
	"github.com/alexedtionweb/niro-memory/metrics"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestConsole(t *testing.T) {
	e, _ := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	keys, _ := api.NewKeys([]api.Key{{Key: "admin-key-0123456789", Tenant: "t", Admin: true}, {Key: "user-key-01234567890", Tenant: "t", User: "u"}})
	srv := httptest.NewServer(devui.Handler(devui.Options{Engine: e, Auth: keys, Metrics: metrics.New()}))
	defer srv.Close()
	do := func(method, path, key, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, sb.String()
	}
	if !devui.Enabled {
		t.Fatal("Enabled must be true with the devui tag")
	}
	if code, body := do("GET", "/_dev/", "", ""); code != 200 || !strings.Contains(body, "dev console") {
		t.Fatalf("index: %d", code)
	}
	if code, _ := do("GET", "/_dev/js/app.js", "", ""); code != 200 {
		t.Fatal("static assets")
	}
	if code, _ := do("GET", "/_dev/api/info", "", ""); code != 401 {
		t.Fatal("unauthenticated dev API")
	}
	if code, _ := do("GET", "/_dev/api/info", "user-key-01234567890", ""); code != 403 {
		t.Fatal("non-admin dev API")
	}
	code, body := do("GET", "/_dev/api/info", "admin-key-0123456789", "")
	var info map[string]any
	json.Unmarshal([]byte(body), &info)
	if code != 200 || info["tenant"] != "t" {
		t.Fatalf("info: %d %s", code, body)
	}
	if code, body := do("POST", "/_dev/api/rules/test", "admin-key-0123456789", `{"rules":{"version":1,"facts":[{"id":"x","pattern":"(?P<n>cat)","emit":{"text":"has {n}"}}]},"text":"I have a cat."}`); code != 200 || !strings.Contains(body, `"text":"has cat"`) {
		t.Fatalf("rules test: %d %s", code, body)
	}
	if code, body := do("POST", "/_dev/api/rules/test", "admin-key-0123456789", `{"rules":{"version":7},"text":"x"}`); code != 200 || !strings.Contains(body, "unsupported version") {
		t.Fatalf("invalid rules must be reported: %d %s", code, body)
	}
	if code, _ := do("POST", "/_dev/api/sql", "admin-key-0123456789", `{"sql":"select 1"}`); code != 400 {
		t.Fatal("memstore has no SQL console")
	}
	resp, _ := http.Get(srv.URL + "/_dev/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("missing CSP: %q", csp)
	}
}
