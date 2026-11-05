package qdrant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPurgeDeletesOnlyTenantInOwnedCollections(t *testing.T) {
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/collections" {
			fmt.Fprint(w, `{"result":{"collections":[{"name":"niromem_model"},{"name":"unrelated"}]}}`)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/collections/niromem_model/points/delete" || r.URL.Query().Get("wait") != "true" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		var body struct {
			Filter struct {
				Must []struct {
					Key   string `json:"key"`
					Match struct {
						Value string `json:"value"`
					} `json:"match"`
				} `json:"must"`
			} `json:"filter"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Filter.Must) != 1 || body.Filter.Must[0].Key != "tenant" || body.Filter.Must[0].Match.Value != "wipe" {
			t.Errorf("unscoped filter %+v", body)
		}
		deletes++
		fmt.Fprint(w, `{"result":true}`)
	}))
	defer server.Close()
	if err := New(Options{URL: server.URL}).PurgeTenant(context.Background(), "wipe"); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("deletes %d", deletes)
	}
}
