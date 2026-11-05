package s3

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPurgeUsesBoundedTenantPrefixAndSurfacesFailure(t *testing.T) {
	deleted := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("unsigned request")
		}
		if r.Method == "GET" {
			if r.URL.Query().Get("prefix") != "niro/wipe/" || r.URL.Query().Get("max-keys") != "2" {
				t.Errorf("unscoped list %s", r.URL)
			}
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>niro/wipe/ab/object</Key></Contents></ListBucketResult>`)
			return
		}
		if r.Method != "DELETE" || r.URL.Path != "/bucket/niro/wipe/ab/object" {
			t.Errorf("unexpected delete %s %s", r.Method, r.URL)
		}
		if fail {
			http.Error(w, "storage down", 503)
			return
		}
		deleted++
		w.WriteHeader(204)
	}))
	defer server.Close()
	s, err := New(Options{Endpoint: server.URL, Region: "local", Bucket: "bucket", Prefix: "niro/", PathStyle: true, AccessKey: "test", SecretKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	n, done, err := s.PurgeTenantBatch(context.Background(), "wipe", 2)
	if err != nil || n != 1 || !done || deleted != 1 {
		t.Fatalf("purge %d %t %v", n, done, err)
	}
	fail = true
	if _, done, err := s.PurgeTenantBatch(context.Background(), "wipe", 2); err == nil || done {
		t.Fatalf("success-shaped failure %t %v", done, err)
	}
}
