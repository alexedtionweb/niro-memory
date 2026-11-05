package api_test

import (
	"net/http/httptest"
	"testing"

	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestTenantCleanupRequiresAdminAndExactTypedTenant(t *testing.T) {
	e, err := niromem.Open(niromem.Options{Store: memstore.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	keys, err := api.NewKeys([]api.Key{{Key: adminKey, Tenant: "acme", Admin: true}, {Key: aliceKey, Tenant: "acme", User: "alice"}, {Key: otherKey, Tenant: "other", Admin: true}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: e, Auth: keys}).Handler())
	defer srv.Close()
	for _, tc := range []struct {
		key, confirmation string
		code              int
	}{{aliceKey, "acme", 403}, {adminKey, "ACME", 400}, {otherKey, "acme", 400}, {adminKey, "acme", 202}} {
		code, out := call(t, srv, tc.key, "POST", "/v1/maintenance/purge", map[string]string{"confirmation": tc.confirmation, "request_id": "test"})
		if code != tc.code {
			t.Fatalf("%d != %d: %v", code, tc.code, out)
		}
	}
}
