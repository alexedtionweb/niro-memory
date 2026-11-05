//go:build devui

package devui

import (
	"bytes"
	"html/template"
	"net/http"
)

type workspaceView struct {
	ID, Title, Description, Default string
	Tabs                            []string
	Tools                           map[string]string
}

var workspaces = map[string]workspaceView{
	"workspace": {"workspace", "Memory workspace", "Browse, remember and search. History and relationships stay connected to your memories.", "explorer",
		[]string{"explorer", "documents", "intelligence", "search", "timeline"},
		map[string]string{"explorer": "Browse", "documents": "Documents & lineage", "intelligence": "Remember", "search": "Search", "timeline": "Timeline",
			"knowledge-state": "Knowledge state", "transitions": "Transition inspector", "memory": "Memory details", "graph": "Entity graph"}},
	"models": {"models", "Models & schemas", "See the actual embedding coverage and configure what the intelligence model extracts.", "embeddings",
		[]string{"embeddings", "schema", "tenants"},
		map[string]string{"embeddings": "Embeddings", "schema": "Extraction schema", "tenants": "Tenant settings"}},
	"operations": {"operations", "Operations", "Inspect background processing and advanced development tools.", "jobs",
		[]string{"jobs", "metrics", "sql"},
		map[string]string{"jobs": "Background jobs", "metrics": "Metrics", "sql": "Read-only SQL", "rules": "Extraction rules",
			"conversations": "Conversations", "add": "Raw ingestion", "memories": "Raw memory browser"}},
}

var workspaceTemplate = template.Must(template.New("workspace").Parse(`
<section class="workspace-shell" data-screen="{{.Screen.ID}}">
  <header class="workspace-heading"><p class="eyebrow">Development workspace</p><h1>{{.Screen.Title}}</h1><p class="sub">{{.Screen.Description}}</p></header>
  <nav class="workspace-tabs" aria-label="Workspace views">
  {{range $id := .Screen.Tabs}}<a href="#/{{$.Screen.ID}}?view={{$id}}" {{if eq $id $.Tool}}aria-current="page" class="active"{{end}}>{{index $.Screen.Tools $id}}</a>{{end}}
  {{if .Extra}}<details class="more-tools"><summary>{{if .Advanced}}{{.Label}}{{else}}More tools{{end}}</summary><div>
  {{range $id, $label := .Extra}}<a href="#/{{$.Screen.ID}}?view={{$id}}" {{if eq $id $.Tool}}aria-current="page"{{end}}>{{$label}}</a>{{end}}
  </div></details>{{end}}
  </nav>
  <div id="workspace-content" data-module="{{.Tool}}" data-focus="{{.Focus}}" aria-live="polite"><p>Loading {{.Label}}…</p></div>
</section>`))

func (d *dev) workspace(w http.ResponseWriter, r *http.Request) {
	id, err := d.o.Auth.Authenticate(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		http.Error(w, "Missing or invalid API key. Check the connection settings.", http.StatusUnauthorized)
		return
	}
	if !id.Principal.Admin {
		http.Error(w, errForbidden.Error(), http.StatusForbidden)
		return
	}
	screen, ok := workspaces[r.PathValue("screen")]
	if !ok {
		http.Error(w, "Unknown workspace.", http.StatusNotFound)
		return
	}
	tool := r.URL.Query().Get("view")
	if tool == "" {
		tool = screen.Default
	}
	label, ok := screen.Tools[tool]
	if !ok {
		http.Error(w, "Unknown view for this workspace.", http.StatusNotFound)
		return
	}
	extra := make(map[string]string)
	for id, name := range screen.Tools {
		primary := false
		for _, tab := range screen.Tabs {
			primary = primary || id == tab
		}
		if !primary {
			extra[id] = name
		}
	}
	var body bytes.Buffer
	if err := workspaceTemplate.Execute(&body, struct {
		Screen             workspaceView
		Tool, Label, Focus string
		Extra              map[string]string
		Advanced           bool
	}{screen, tool, label, r.URL.Query().Get("focus"), extra, extra[tool] != ""}); err != nil {
		http.Error(w, "Unable to render workspace.", http.StatusInternalServerError)
		return
	}
	w.Write(body.Bytes())
}
