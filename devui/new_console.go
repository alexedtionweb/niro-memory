//go:build devui

package devui

import (
	"embed"
	"html/template"
)

//go:embed templates
var consoleTemplateFiles embed.FS

var consoleTemplates = template.Must(template.ParseFS(consoleTemplateFiles,
	"templates/layout_console.html",
	"templates/partials/*.html",
))
