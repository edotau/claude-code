package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// Scanner audits the config surface for inline, plaintext secrets. Findings accumulate across
// files; Out receives the human-readable report. Values are never echoed, only their location.
type Scanner struct {
	Out      io.Writer
	Findings []string
}

func (s *Scanner) out() io.Writer {
	if s.Out != nil {
		return s.Out
	}
	return os.Stdout
}

func (s *Scanner) flag(detail string) {
	s.Findings = append(s.Findings, detail)
	fmt.Fprintf(s.out(), "  ⚠ %s\n", detail)
}

func (s *Scanner) ok(msg string) { fmt.Fprintf(s.out(), "  ✓ %s\n", msg) }

// DefaultScanTargets is the config surface scanned when `secrets scan` is given no paths: the
// JSON configs Claude Code and this harness read, plus the KEY=value pin file (never secrets.env
// itself — that file is the designated secret store, not a finding).
func DefaultScanTargets() []string {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	out := []string{
		filepath.Join(paths.ConfigDir(), "settings.json"),
		filepath.Join(paths.ConfigDir(), "settings.local.json"),
		providers.UserFile(), // providers.json
		paths.ClaudeJSON(),   // ~/.claude.json
		filepath.Join(paths.EnvDir(), "provider.env"),
	}
	if home != "" {
		out = append(out, filepath.Join(home, ".mcp.json"))
	}
	if cwd != "" {
		out = append(out, filepath.Join(cwd, ".mcp.json"))
	}
	return dedupExisting(out)
}

// dedupExisting drops duplicates (by resolved path) and anything that doesn't exist, preserving order.
func dedupExisting(files []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, p := range files {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		rp := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			rp = r
		}
		if seen[rp] {
			continue
		}
		seen[rp] = true
		out = append(out, p)
	}
	return out
}

// walkJSONStrings visits every string leaf of parsed JSON with its dotted path.
func walkJSONStrings(prefix string, node any, visit func(path, val string)) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			walkJSONStrings(join(k), child, visit)
		}
	case []any:
		for i, child := range v {
			walkJSONStrings(join(strconv.Itoa(i)), child, visit)
		}
	case string:
		visit(prefix, v)
	}
}

// ScanFile inspects one config file: JSON leaves when it parses as JSON, else KEY=value lines (so
// the one scanner also covers env.d/provider.env). A missing file is silently fine.
func (s *Scanner) ScanFile(path string) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.flag(fmt.Sprintf("%s — could not read: %v", path, err))
		return
	}
	before := len(s.Findings)
	var doc any
	if json.Unmarshal(raw, &doc) == nil {
		walkJSONStrings("", doc, func(p, val string) {
			if LooksLikeSecret(val) {
				s.flag(fmt.Sprintf("%s → %s holds an inline secret", path, p))
			}
		})
	} else {
		for k, v := range providers.ReadEnvFile(path) {
			if LooksLikeSecret(v) {
				s.flag(fmt.Sprintf("%s → %s holds an inline secret", path, k))
			}
		}
	}
	if len(s.Findings) == before {
		s.ok(path + " — clean")
	}
}

// ScanConfigs audits targets (DefaultScanTargets when empty) and prints the summary line.
func (s *Scanner) ScanConfigs(targets []string) int {
	if len(targets) == 0 {
		targets = DefaultScanTargets()
	}
	fmt.Fprintln(s.out(), "== config-surface credential scan ==")
	for _, f := range targets {
		s.ScanFile(f)
	}
	if n := len(s.Findings); n > 0 {
		fmt.Fprintf(s.out(), "secrets scan: %d finding(s) — move inline values to env.d/secrets.env or the keyring.\n", n)
		return ExitFindings
	}
	fmt.Fprintln(s.out(), "secrets scan: clean.")
	return ExitClean
}
