package web

import (
	"embed"
	"html/template"
)

//go:embed assets/index.html.tmpl
var assetsFS embed.FS

// indexTemplate is the dashboard page: inline CSS/JS, no external assets, parsed once at init.
var indexTemplate = template.Must(template.ParseFS(assetsFS, "assets/index.html.tmpl"))
