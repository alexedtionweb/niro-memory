//go:build devui

package devui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/devui"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestDeadJobRetryIsIdempotentAndTenantScoped(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(nil)
	e, err := niromem.Open(niromem.Options{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{
		{Kind: niromem.JobKnowledgeEvaluate, Key: "source", Payload: json.RawMessage(`{"id":"source"}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, "worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	job := claimed[0]
	if err := s.Finish(ctx, job.ID, "worker", store.Outcome{Dead: true, Error: "provider timed out"}); err != nil {
		t.Fatal(err)
	}
	keys, err := api.NewKeys([]api.Key{{Key: "admin-key-0123456789", Tenant: "t", Admin: true},
		{Key: "other-key-0123456789", Tenant: "other", Admin: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := devui.Handler(devui.Options{Engine: e, Auth: keys})
	retry := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/_dev/api/jobs/retry", strings.NewReader(`{"id":"`+job.ID+`"}`))
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := retry("other-key-0123456789"); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant retry: %d %s", w.Code, w.Body)
	}
	for _, replayed := range []bool{false, true} {
		w := retry("admin-key-0123456789")
		var result struct {
			Replayed bool `json:"replayed"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Replayed != replayed {
			t.Fatalf("retry: %d %s %v", w.Code, w.Body, err)
		}
	}
	pending, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", State: store.JobPending})
	if err != nil || len(pending) != 1 || pending[0].ID != job.ID || string(pending[0].Payload) != string(job.Payload) || pending[0].Key != job.Key {
		t.Fatalf("retry duplicated or changed durable work: %+v %v", pending, err)
	}
}
