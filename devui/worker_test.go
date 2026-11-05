//go:build devui

package devui_test

import (
	"context"
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

func TestManualWorkerDoesNotCompeteOrOverclaim(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[inline], func(t *testing.T) {
			ctx := context.Background()
			s := memstore.New(nil)
			e, err := niromem.Open(niromem.Options{Store: s})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if _, err := s.Write(ctx, &store.Batch{Tenant: "t", Jobs: []store.Job{
				{Kind: "unknown", Key: "one"}, {Kind: "unknown", Key: "two"},
			}}); err != nil {
				t.Fatal(err)
			}
			handler := devui.Handler(devui.Options{Engine: e, Auth: api.Dev{}, InlineWorker: inline})
			request := httptest.NewRequest(http.MethodPost, "/_dev/api/jobs/run", nil)
			request.Header.Set("X-Tenant", "t")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)
			wantPending := 1
			if inline {
				wantPending = 2
				if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "processed automatically") {
					t.Fatalf("manual pass competed with automatic worker: %d %s", w.Code, w.Body)
				}
			} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"processed":1`) {
				t.Fatalf("bounded manual pass: %d %s", w.Code, w.Body)
			}
			pending, err := s.Jobs(ctx, store.JobQuery{Tenant: "t", State: store.JobPending})
			if err != nil || len(pending) != wantPending {
				t.Fatalf("overclaimed queue: %+v %v", pending, err)
			}
		})
	}
}
