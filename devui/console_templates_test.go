//go:build devui

package devui

import (
	"bytes"
	"strings"
	"testing"
)

func TestConsolePartialsRenderAccessibleContent(t *testing.T) {
	tests := []struct {
		name string
		data any
		want string
	}{
		{"badge", map[string]any{"Kind": "ok", "Text": "Current"}, "Current"},
		{"stat", map[string]any{"Label": "Semantic coverage", "Value": "20%", "Subline": "20 of 88 sections", "ActionURL": "/embed", "ActionLabel": "Complete embeddings"}, "20 of 88 sections"},
		{"capability_row", map[string]any{"State": "partial", "Title": "Semantic search", "Explanation": "Some sections are indexed."}, "Some sections are indexed."},
		{"empty_state", map[string]any{"Title": "No structured knowledge yet", "Consequence": "Agents cannot reason over individual facts.", "ActionURL": "/extract", "ActionLabel": "Run extraction"}, "Run extraction"},
		{"tech_details", map[string]any{"Fields": []any{map[string]string{"Key": "id", "Value": "memory-1"}}, "RawJSON": `{"id":"memory-1"}`, "RawID": "raw-1"}, "Technical details"},
		{"action_result", map[string]any{"Title": "New memory created", "Sentence": "This is a new independent memory.", "Tech": map[string]any{"Fields": []any{}}}, "This is a new independent memory."},
		{"progress_bar", map[string]any{"Value": 20, "Total": 88, "Label": "Sections with semantic embeddings"}, "20 / 88"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := consoleTemplates.ExecuteTemplate(&out, test.name, test.data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("rendered partial %q does not contain %q", out.String(), test.want)
			}
		})
	}
}

func TestConsoleLayoutHasResponsiveAppNavigation(t *testing.T) {
	var out bytes.Buffer
	data := map[string]any{
		"Title": "Overview", "Tenant": "dev", "HealthClass": "health-healthy", "HealthLabel": "Healthy",
		"Active": "overview", "Breadcrumbs": []any{map[string]string{"Text": "Overview"}},
	}
	if err := consoleTemplates.ExecuteTemplate(&out, "layout_console", data); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"niromem", "Search memories", "aria-disabled=\"true\"", "Classic view"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("layout is missing %q", text)
		}
	}
}
