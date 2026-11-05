//go:build devui

package devui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/devui"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestJobControlsAreAdminOnlyAndTenantScoped(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	e, err := niromem.Open(niromem.Options{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{{Kind: "slow", Key: "source"}}}); err != nil {
		t.Fatal(err)
	}
	js, err := s.Jobs(ctx, store.JobQuery{Tenant: "t"})
	if err != nil || len(js) != 1 {
		t.Fatalf("seed %+v %v", js, err)
	}
	keys, err := api.NewKeys([]api.Key{{Key: "admin-test-key-00001", Tenant: "t", Admin: true}, {Key: "other-test-key-00001", Tenant: "other", Admin: true}, {Key: "user-test-key-000001", Tenant: "t", User: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := devui.Handler(devui.Options{Engine: e, Auth: keys})
	control := func(key, action string, priority int) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"id": js[0].ID, "action": action, "priority": priority})
		r := httptest.NewRequest(http.MethodPost, "/_dev/api/jobs/control", strings.NewReader(string(body)))
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		key, action    string
		priority, code int
	}{
		{"user-test-key-000001", "pause", 0, 403},
		{"other-test-key-00001", "pause", 0, 404},
		{"admin-test-key-00001", "priority", 101, 400},
		{"admin-test-key-00001", "priority", 50, 200},
		{"admin-test-key-00001", "pause", 0, 200},
		{"admin-test-key-00001", "resume", 0, 200},
		{"admin-test-key-00001", "cancel", 0, 200},
		{"admin-test-key-00001", "delete", 0, 200},
	} {
		if w := control(tc.key, tc.action, tc.priority); w.Code != tc.code {
			t.Fatalf("%s %d %s", tc.action, w.Code, w.Body)
		}
	}
	if remaining, err := s.Jobs(ctx, store.JobQuery{Tenant: "t"}); err != nil || len(remaining) != 0 {
		t.Fatalf("delete retained job %+v %v", remaining, err)
	}
}
