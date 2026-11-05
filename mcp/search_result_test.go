package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/search"
)

func TestSearchOutputBudgetIsExactAndTruncationExplicit(t *testing.T) {
	native := search.Result{Trace: []search.Step{{Strategy: "vector", Ran: true, Candidates: 60}}}
	for i := 0; i < 100; i++ {
		native.Hits = append(native.Hits, search.Hit{Score: .7, Memory: memory.Memory{ID: memory.NewID(), Text: strings.Repeat("\u00e7", 4000), Metadata: map[string]any{"document_id": "document", "section": "5.2.1.1", "huge": strings.Repeat("x", 7000)}}})
	}
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"compact", "full"} {
		out, err := boundedSearchResult(raw, mode, 1200)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(out)
		if err != nil || len(data) > searchResultBytes || !out.Truncated || out.Omitted == 0 || len(out.Hits) == 0 || len(out.Hits)+out.Omitted != 100 || len(out.Trace) != 1 {
			t.Fatalf("unbounded/incomplete result %s: %d bytes, %+v %v", mode, len(data), out, err)
		}
		if mode == "compact" {
			var hit compactHit
			if err := json.Unmarshal(out.Hits[0], &hit); err != nil {
				t.Fatal(err)
			}
			if !hit.Memory.TextTruncated || utf8.RuneCountInString(hit.Memory.Text) != 1200 || hit.Memory.References["section"] != "5.2.1.1" || strings.Contains(string(data), `"huge"`) {
				t.Fatal("preview not compact or source references lost")
			}
		}
	}
}

func TestWorkspaceSearchDefaultsCompactAndFullIsExplicit(t *testing.T) {
	f := newWorkspaceFixture(t)
	for i := 0; i < 8; i++ {
		workspaceCall(t, f.admin, "remember_memory", map[string]any{"text": strings.Repeat("Convention ", 140) + memory.NewID()}, false)
	}
	raw := workspaceCall(t, f.admin, "search_memory", map[string]any{"query": "Convention"}, false)
	var out boundedSearch
	if err := json.Unmarshal(raw, &out); err != nil || out.Mode != "compact" || len(out.Hits) != 5 || !out.Truncated {
		t.Fatalf("defaults: %s (%v)", raw, err)
	}
	var h compactHit
	if err := json.Unmarshal(out.Hits[0], &h); err != nil || h.Explain != nil {
		t.Fatalf("explanation unexpectedly enabled: %s (%v)", raw, err)
	}
	full := workspaceCall(t, f.admin, "search_memory", map[string]any{"query": "Convention", "response_mode": "full", "explain": true, "limit": 1}, false)
	if !strings.Contains(string(full), `"found_by"`) || !strings.Contains(string(full), `"source"`) {
		t.Fatalf("full opt-in not honored: %s", full)
	}
	workspaceCall(t, f.admin, "search_memory", map[string]any{"query": "Convention", "text_max_chars": 0}, true)
}
