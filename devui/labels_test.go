//go:build devui

package devui

import (
	"testing"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/search"
)

func TestReconcilerActionLabels(t *testing.T) {
	tests := []struct {
		action   memory.Action
		title    string
		sentence string
	}{
		{memory.ActionAdd, "New memory created", "This is a new independent memory."},
		{memory.ActionEvidence, "Existing memory reinforced", "Same content already existed; this source was added as evidence."},
		{memory.ActionSupersede, "Current value updated", "Previous value closed and kept in history."},
		{memory.ActionUpdate, "Earlier value corrected", "The previous value was marked as wrong, not deleted."},
		{memory.ActionInvalidate, "Current value ended", "This property no longer has a current value."},
		{"history", "history", "(unmapped action)"},
	}
	for _, test := range tests {
		t.Run(string(test.action), func(t *testing.T) {
			got := ReconcilerActionLabel(test.action)
			if got.Title != test.title || got.Sentence != test.sentence {
				t.Fatalf("label = %+v, want %q / %q", got, test.title, test.sentence)
			}
		})
	}
}

func TestSearchTraceDescriptions(t *testing.T) {
	tests := []struct {
		name string
		step search.Step
		want string
		link string
	}{
		{"missing embedder", search.Step{Strategy: "vector", Reason: "no query vector and no embedder"}, "Semantic search is off: no embedding model configured. Only shared words and known entities match; a query in another language may miss results.", "Configure semantic search"},
		{"budget", search.Step{Strategy: "vector", Reason: "skipped: embedding exceeded budget"}, "Semantic search skipped: the query embedding was not ready within the policy time limit.", ""},
		{"ran", search.Step{Strategy: "lexical", Ran: true, Candidates: 7}, "Keyword search found 7 candidates.", ""},
		{"unknown reason", search.Step{Strategy: "future", Reason: "new strategy reason"}, "new strategy reason", ""},
		{"unknown skipped", search.Step{Strategy: "future"}, "future did not run.", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SearchTraceDescription(test.step)
			if got.Sentence != test.want || got.LinkText != test.link {
				t.Fatalf("description = %+v, want %q and link %q", got, test.want, test.link)
			}
			if test.link != "" && got.LinkURL == "" {
				t.Fatal("semantic-search configuration link is missing")
			}
		})
	}
}

func TestMemoryKindAndLayerLabels(t *testing.T) {
	for kind, want := range map[memory.Kind]string{
		memory.Working: "Short-term", memory.Episodic: "Event", memory.Semantic: "Fact", memory.Procedural: "Procedure",
	} {
		if got := MemoryKindLabel(kind); got != want {
			t.Errorf("kind %q = %q, want %q", kind, got, want)
		}
	}
	for layer, want := range map[memory.Layer]string{
		memory.Atom: "Atom", memory.Scene: "Scene", memory.Profile: "Profile",
	} {
		if got := MemoryLayerLabel(layer); got != want {
			t.Errorf("layer %d = %q, want %q", layer, got, want)
		}
	}
}
