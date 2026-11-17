package agent

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

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

// RenderBrief resolves report paths against the memory bank.
func RenderBrief(input BriefInput) (string, error) {
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
		OwnedLine, ReportsDir string
	}{input, string(owned), memory.SubDir("reports")}
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

var briefDir = os.TempDir
var briefRandom = rand.Reader
var briefAgent = regexp.MustCompile(`^[a-zA-Z0-9_-]*$`)
var briefName = regexp.MustCompile(`^claude-code-brief-[0-9]{8}(?:-[a-zA-Z0-9_-]+)?-[0-9a-f]{32}\.md$`)

// NewBriefPath reserves an empty private file before returning its absolute path.
func NewBriefPath(name string) (string, error) {
	if !briefAgent.MatchString(name) {
		return "", fmt.Errorf("invalid brief agent %q", name)
	}

	dir, err := filepath.Abs(briefDir())
	if err != nil {
		return "", fmt.Errorf("brief directory: %w", err)
	}
	now := time.Now()
	if err := pruneBriefs(dir, now); err != nil {
		return "", err
	}
	prefix := "claude-code-brief-" + now.Format("20060102")
	if name != "" {
		prefix += "-" + name
	}
	return reserveBrief(dir, prefix)
}

func reserveBrief(dir, prefix string) (string, error) {
	for {
		var random [16]byte
		if _, err := io.ReadFull(briefRandom, random[:]); err != nil {
			return "", fmt.Errorf("brief randomness: %w", err)
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%x.md", prefix, random))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create brief: %w", err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close brief: %w", err)
		}
		return path, nil
	}
}

func pruneBriefs(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list briefs: %w", err)
	}
	for _, entry := range entries {
		if !briefName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat brief: %w", err)
		}
		if !info.ModTime().Before(now.Add(-7 * 24 * time.Hour)) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("prune brief: %w", err)
		}
	}
	return nil
}
