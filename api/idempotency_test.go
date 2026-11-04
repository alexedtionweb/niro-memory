package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func callWithOperationKey(t *testing.T, srv *httptest.Server, token, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestLegacyMutationIdempotencyHeaders(t *testing.T) {
	srv := server(t)
	facts := map[string]any{"facts": []any{map[string]any{"text": "The project uses Go", "slot": "project.language"}}}
	code, first := callWithOperationKey(t, srv, aliceKey, "POST", "/v1/facts", "legacy-fact", facts)
	if code != http.StatusOK {
		t.Fatalf("remember: %d %v", code, first)
	}
	id := first["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)["id"].(string)
	code, replayed := callWithOperationKey(t, srv, aliceKey, "POST", "/v1/facts", "legacy-fact", facts)
	if code != http.StatusOK || replayed["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)["id"] != id {
		t.Fatalf("remember replay: %d %v", code, replayed)
	}
	changed := map[string]any{"facts": []any{map[string]any{"text": "The project uses Rust", "slot": "project.language"}}}
	if code, out := callWithOperationKey(t, srv, aliceKey, "POST", "/v1/facts", "legacy-fact", changed); code != http.StatusConflict {
		t.Fatalf("remember mismatch: %d %v", code, out)
	}
	conflicting := map[string]any{"facts": []any{map[string]any{"text": "Other fact", "operation_key": "body-key"}}}
	if code, out := callWithOperationKey(t, srv, aliceKey, "POST", "/v1/facts", "header-key", conflicting); code != http.StatusBadRequest {
		t.Fatalf("conflicting remember key: %d %v", code, out)
	}

	patch := map[string]any{"text": "The project uses Go 1.23"}
	path := "/v1/memories/" + id
	code, updated := callWithOperationKey(t, srv, aliceKey, "PATCH", path, "legacy-update", patch)
	if code != http.StatusOK {
		t.Fatalf("update: %d %v", code, updated)
	}
	if code, out := callWithOperationKey(t, srv, aliceKey, "PATCH", path, "legacy-update", patch); code != http.StatusOK || out["id"] != updated["id"] || out["version"] != updated["version"] {
		t.Fatalf("update replay: %d %v", code, out)
	}
	if code, out := callWithOperationKey(t, srv, aliceKey, "PATCH", path, "legacy-update", map[string]any{"text": "Different correction"}); code != http.StatusConflict {
		t.Fatalf("update mismatch: %d %v", code, out)
	}
	if code, out := callWithOperationKey(t, srv, aliceKey, "PATCH", path, "header-key", map[string]any{"text": "Other correction", "operation_key": "body-key"}); code != http.StatusBadRequest {
		t.Fatalf("conflicting update key: %d %v", code, out)
	}

	currentPath := "/v1/memories/" + updated["id"].(string)
	invalidate := map[string]any{"reason": "No longer current"}
	for attempt := 0; attempt < 2; attempt++ {
		if code, out := callWithOperationKey(t, srv, aliceKey, "POST", currentPath+"/invalidate", "legacy-invalidate", invalidate); code != http.StatusOK {
			t.Fatalf("invalidate attempt %d: %d %v", attempt, code, out)
		}
	}
	if code, out := callWithOperationKey(t, srv, aliceKey, "POST", currentPath+"/invalidate", "legacy-invalidate", map[string]any{"reason": "Changed reason"}); code != http.StatusConflict {
		t.Fatalf("invalidate mismatch: %d %v", code, out)
	}

	for attempt := 0; attempt < 2; attempt++ {
		if code, out := callWithOperationKey(t, srv, aliceKey, "DELETE", currentPath, "legacy-delete", nil); code != http.StatusOK {
			t.Fatalf("delete attempt %d: %d %v", attempt, code, out)
		}
	}
	if code, out := callWithOperationKey(t, srv, aliceKey, "DELETE", "/v1/memories/missing", "legacy-delete", nil); code != http.StatusConflict {
		t.Fatalf("delete receipt input mismatch: %d %v", code, out)
	}
	if code, out := callWithOperationKey(t, srv, aliceKey, "DELETE", currentPath, "fresh-delete-key", nil); code != http.StatusNotFound {
		t.Fatalf("deleted record without receipt: %d %v", code, out)
	}
}

func TestDeleteRetryDoesNotBypassFreshACL(t *testing.T) {
	srv := server(t)
	code, seeded := call(t, srv, adminKey, "POST", "/v1/facts", map[string]any{
		"facts": []any{map[string]any{
			"text": "Bob's private credential-free preference", "user_id": "bob", "visibility": "private",
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("seed private record: %d %v", code, seeded)
	}
	id := seeded["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)["id"].(string)
	path := "/v1/memories/" + id
	if code, out := callWithOperationKey(t, srv, aliceKey, "DELETE", path, "unseen-delete-key", nil); code != http.StatusNotFound {
		t.Fatalf("fresh delete bypassed ACL: %d %v", code, out)
	}
	if code, out := call(t, srv, adminKey, "GET", path, nil); code != http.StatusOK {
		t.Fatalf("private record was removed: %d %v", code, out)
	}
}
