package web

import (
	"embed"
	"html/template"
	"sync"
)

//go:embed assets/index.html.tmpl
var assetsFS embed.FS

// indexTemplate is the dashboard page: inline CSS/JS, no external assets, parsed once on first use.
var indexTemplate = sync.OnceValue(func() *template.Template { return template.Must(template.ParseFS(assetsFS, "assets/index.html.tmpl")) })
