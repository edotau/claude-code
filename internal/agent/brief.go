package agent

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// BriefInput supplies the authored task and the leaf's execution contract.
type BriefInput struct {
	Task     string
	Owned    []string
	ReadOnly bool
	Report   string
	Verify   string
}

//go:embed brief.tmpl
var briefTemplate string

// RenderBrief renders a deterministic brief without reading files or changing its input.
func RenderBrief(input BriefInput) (string, error) {
	owned, err := json.Marshal(input.Owned)
	if err != nil {
		return "", fmt.Errorf("encode brief ownership: %w", err)
	}
	tmpl, err := template.New("brief").Parse(briefTemplate)
	if err != nil {
		return "", fmt.Errorf("parse brief template: %w", err)
	}
	var out bytes.Buffer
	data := struct {
		BriefInput
		OwnedLine string
	}{input, string(owned)}
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render brief: %w", err)
	}
	return out.String(), nil
}

// ParseOwned reads the first Owned: line, accepting rendered JSON or legacy whitespace-separated paths.
func ParseOwned(brief string) []string {
	for line := range strings.SplitSeq(brief, "\n") {
		value, ok := strings.CutPrefix(line, "Owned:")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "[") {
			var owned []string
			if json.Unmarshal([]byte(value), &owned) != nil {
				return nil
			}
			return owned
		}
		return strings.Fields(value)
	}
	return nil
}
