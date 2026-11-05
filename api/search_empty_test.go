package api_test

import (
	"testing"
)

func TestEmptySearchAndContextCollectionsAreArrays(t *testing.T) {
	srv := server(t)
	for _, body := range []map[string]any{
		{},
		{"ids": []string{"missing-memory"}},
		{"query": "nonexistent knowledge"},
	} {
		code, result := call(t, srv, aliceKey, "POST", "/v1/search", body)
		if code != 200 {
			t.Fatalf("search: %d %+v", code, result)
		}
		for _, field := range []string{"hits", "trace"} {
			items, ok := result[field].([]any)
			if !ok || field == "hits" && len(items) != 0 {
				t.Fatalf("search %v: %s must be an array, got %#v", body, field, result[field])
			}
		}
	}
	for _, body := range []map[string]any{{}, {"query": "nonexistent knowledge"}} {
		code, result := call(t, srv, aliceKey, "POST", "/v1/context", body)
		if code != 200 {
			t.Fatalf("context: %d %+v", code, result)
		}
		c, ok := result["context"].(map[string]any)
		if !ok {
			t.Fatalf("missing context: %+v", result)
		}
		for _, field := range []string{"profile", "scenes", "facts", "skills"} {
			items, ok := c[field].([]any)
			if !ok || len(items) != 0 {
				t.Fatalf("empty context %s must be [], got %#v", field, c[field])
			}
		}
	}
}
