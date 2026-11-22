package bench

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed briefs/fix.tmpl
var fixBriefTemplate string

//go:embed briefs/review.tmpl
var reviewBriefTemplate string

// RenderBrief accepts fix or review; callers supply complete rtk commands and a JSON OwnedLine.
func RenderBrief(kind string, values map[string]string) (string, error) {
	var source string
	switch kind {
	case "fix":
		source = fixBriefTemplate
	case "review":
		source = reviewBriefTemplate
	default:
		return "", fmt.Errorf("unknown bench brief %q", kind)
	}
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("bench brief %s: empty %s", kind, key)
		}
	}
	funcs := template.FuncMap{"sh": func(s string) string {
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}}
	tmpl, err := template.New(kind).Funcs(funcs).Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse bench brief %s: %w", kind, err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, values); err != nil {
		return "", fmt.Errorf("render bench brief %s: %w", kind, err)
	}
	return out.String(), nil
}
