package agent

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/edotau/claude-code/internal/memory"
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

// RenderBrief resolves report paths against the current working directory's bank.
func RenderBrief(input BriefInput) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve brief working directory: %w", err)
	}
	b := memory.BankFor(cwd)
	if input.Owned == nil {
		input.Owned = []string{}
	}
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
		OwnedLine, ReportsDir, Prefix string
	}{input, string(owned), b.SubDir("reports"), b.Prefix()}
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
		if value == "null" {
			return nil
		}
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

// ReadBrief returns the brief file's text; "" is an empty brief and "-" reads stdin.
func ReadBrief(path string) (string, error) {
	switch path {
	case "":
		return "", nil
	case "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}
