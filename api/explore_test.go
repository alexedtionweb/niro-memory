package api_test

import (
	"testing"
	"time"
)

func TestExploreIncludesExpiredOnlyWhenRequested(t *testing.T) {
	srv := server(t)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	code, out := call(t, srv, adminKey, "POST", "/v1/facts", map[string]any{
		"facts": []map[string]any{{"text": "Temporary fixture", "expires_at": expires}},
	})
	if code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}

	id := out["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)["id"].(string)
	code, out = call(t, srv, adminKey, "PATCH", "/v1/memories/"+id, map[string]any{
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
	})
	if code != 200 {
		t.Fatalf("expire: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "GET", "/v1/explore", nil)
	if code != 200 || len(out["memories"].([]any)) != 0 {
		t.Fatalf("expired memory must be hidden by default: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "GET", "/v1/explore?show_expired=true", nil)
	if code != 200 || len(out["memories"].([]any)) != 1 {
		t.Fatalf("explicit expired lookup: %d %v", code, out)
	}
	code, out = call(t, srv, otherKey, "GET", "/v1/explore?show_expired=true&all_versions=true", nil)
	if code != 200 || len(out["memories"].([]any)) != 0 {
		t.Fatalf("history must remain tenant isolated: %d %v", code, out)
	}
}

func TestEntityNeighborhoodEmptyCollectionsAreArrays(t *testing.T) {
	srv := server(t)
	code, out := call(t, srv, adminKey, "POST", "/v1/facts", map[string]any{
		"facts": []map[string]any{{"text": "Atlas is a project.", "entities": []map[string]any{{"name": "Atlas", "type": "project"}}}},
	})
	if code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	id := out["changes"].([]any)[0].(map[string]any)["memory"].(map[string]any)["id"].(string)
	code, out = call(t, srv, adminKey, "GET", "/v1/entities/Atlas", nil)
	if code != 200 {
		t.Fatalf("neighbors: %d %v", code, out)
	}
	if related, ok := out["related"].([]any); !ok || len(related) != 0 {
		t.Fatalf("related must be []: %v", out)
	}
	if code, out := call(t, srv, adminKey, "POST", "/v1/memories/"+id+"/invalidate", map[string]any{"reason": "fixture"}); code != 200 {
		t.Fatalf("invalidate: %d %v", code, out)
	}
	code, out = call(t, srv, adminKey, "GET", "/v1/entities/Atlas", nil)
	if code != 200 {
		t.Fatalf("retired neighbors: %d %v", code, out)
	}
	for _, field := range []string{"memories", "related"} {
		if items, ok := out[field].([]any); !ok || len(items) != 0 {
			t.Fatalf("%s must be []: %v", field, out)
		}
	}
}
