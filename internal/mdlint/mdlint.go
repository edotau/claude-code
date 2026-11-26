// Package mdlint is the harness Markdown standard: wrap width, config files and the mdsmith runner.
package mdlint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// Width is the column cap; .markdownlint.json MD013 and .mdsmith.yml line-length must equal it (Parity).
const Width = 120

const (
	RulesFile   = ".markdownlint.json"       // rules: VS Code, markdownlint-cli2, mdsmith init
	CLI2File    = ".markdownlint-cli2.jsonc" // ignores only
	MdsmithFile = ".mdsmith.yml"
)

// ErrNoMdsmith means no mdsmith binary was found.
var ErrNoMdsmith = errors.New("mdlint: mdsmith binary not found")

type Finding struct {
	Path    string `json:"file"`
	Line    int    `json:"line"`
	Rule    string `json:"rule"` // MDS001
	Name    string `json:"name"` // line-length
	Message string `json:"message"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: mdsmith/%s: %s", f.Path, f.Line, f.Rule, f.Message)
}

// Root is the nearest ancestor of path holding .mdsmith.yml, or "".
func Root(path string) string {
	dir, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, MdsmithFile)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Mdsmith is ConfigDir/bin/mdsmith (make mdsmith pins it), else mdsmith on PATH, else "".
func Mdsmith() string {
	p := filepath.Join(paths.ConfigDir(), "bin", "mdsmith")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	if p, err := exec.LookPath("mdsmith"); err == nil {
		return p
	}
	return ""
}

func run(ctx context.Context, root, stdin string, args ...string) ([]byte, []byte, error) {
	bin := Mdsmith()
	if bin == "" {
		return nil, nil, ErrNoMdsmith
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	proc.OwnGroup(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, nil, fmt.Errorf("mdlint: %w", err)
	}
	if err != nil && out.Len() == 0 {
		return nil, nil, fmt.Errorf("mdlint: mdsmith %v: %w: %s", args, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), errb.Bytes(), nil
}

func decode(out []byte) ([]Finding, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var fs []Finding
	if err := json.Unmarshal(out, &fs); err != nil {
		return nil, fmt.Errorf("mdlint: decode findings: %w", err)
	}
	return fs, nil
}

// CheckFile lints rel from cwd root, the only form in which .mdsmith.yml overrides apply.
func CheckFile(ctx context.Context, root, rel string) ([]Finding, error) {
	out, _, err := run(ctx, root, "", "check", "-f", "json", "-o", "-", "--no-color", "--", rel)
	if err != nil {
		return nil, err
	}
	return decode(out)
}

// FixFile rewrites rel in place with mdsmith's autofixes.
func FixFile(ctx context.Context, root, rel string) error {
	_, _, err := run(ctx, root, "", "fix", "--no-color", "--", rel)
	return err
}

// CheckText lints text on stdin against root's rules; overrides never apply to stdin.
func CheckText(ctx context.Context, root, text string) ([]Finding, error) {
	if text == "" {
		text = "\n"
	}
	out, _, err := run(ctx, root, text, "check", "-f", "json", "-o", "-", "--no-color", "-")
	if err != nil {
		return nil, err
	}
	return decode(out)
}

var itemPrefix = regexp.MustCompile(`^(\s*(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?)`)

// WrapItem wraps a line to Width, never splitting a code span or link; continuations indent to the text column.
func WrapItem(line string) []string {
	if len([]rune(line)) <= Width {
		return []string{line}
	}
	prefix := itemPrefix.FindString(line)
	indent := strings.Repeat(" ", len([]rune(prefix)))
	if prefix == "" {
		indent = leadingSpace(line)
	}
	words := atoms(strings.TrimPrefix(line, prefix))
	var lines []string
	cur := prefix
	empty := true
	for _, w := range words {
		if !empty && len([]rune(cur))+1+len([]rune(w)) > Width {
			lines = append(lines, cur)
			cur, empty = indent+w, false
			continue
		}
		if empty {
			cur += w
		} else {
			cur += " " + w
		}
		empty = false
	}
	return append(lines, cur)
}

func leadingSpace(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// atoms splits on spaces outside backtick spans and link text/targets.
func atoms(s string) []string {
	var out []string
	start, tick, depth := 0, false, 0
	for i, r := range s {
		tick, depth = track(r, tick, depth)
		if r == ' ' && !tick && depth == 0 {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func track(r rune, tick bool, depth int) (bool, int) {
	switch {
	case r == '`':
		return !tick, depth
	case tick:
		return tick, depth
	case r == '[' || r == '(':
		return tick, depth + 1
	case (r == ']' || r == ')') && depth > 0:
		return tick, depth - 1
	}
	return tick, depth
}

// StoreRules is the child markdownlint config for generated bank files under base (a dir relative to ConfigDir).
func StoreRules(base string) []byte {
	up := strings.Repeat("../", len(strings.FieldsFunc(filepath.ToSlash(filepath.Clean(base)), func(r rune) bool { return r == '/' })))
	b, _ := json.MarshalIndent(map[string]any{"extends": up + RulesFile, "MD046": false}, "", "  ")
	return append(b, '\n')
}

// WriteStoreConfig writes StoreRules("memory") to dir/.markdownlint.json when its bytes differ (atomic write).
func WriteStoreConfig(dir string) error {
	dest := filepath.Join(dir, RulesFile)
	want := StoreRules("memory")
	if have, err := os.ReadFile(dest); err == nil && bytes.Equal(have, want) {
		return nil
	}
	return paths.AtomicWrite(dest, want, 0o644)
}
