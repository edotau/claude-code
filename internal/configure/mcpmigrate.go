// mcpmigrate.go — rewrites the old `claude-code web search` MCP registration to `network websearch`.
package configure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Old-form argv matchers: opencode's command array, codex's `args = [...]`.
var (
	opencodeOldArgs = regexp.MustCompile(`("command"\s*:\s*\[\s*"(?:[^"]*/)?claude-code"\s*,\s*)"web"(\s*,\s*)"search"(\s*\])`)
	codexTable      = regexp.MustCompile(`(?m)^\[mcp_servers\.[^\]\n]+\]\n(?:[^\[\n].*\n|\n)*`)
	codexCommand    = regexp.MustCompile(`(?m)^command\s*=\s*"([^"]*)"`)
	codexOldArgs    = regexp.MustCompile(`(?m)^([ \t]*args\s*=\s*\[\s*)"web"(\s*,\s*)"search"(\s*\])`)
)

const newArgsRepl = `${1}"network"${2}"websearch"${3}`

// MigrateWebSearchArgs rewrites registrations with args ["web","search"] to ["network","websearch"]
// in ~/.claude.json, ~/.codex/config.toml and opencode.json; one line per file changed, absent files skipped.
func MigrateWebSearchArgs(dryRun bool, out io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	targets := []struct {
		path    string
		rewrite func([]byte) []byte
	}{
		{paths.ClaudeJSON(), rewriteClaudeJSONArgs},
		{filepath.Join(home, ".codex", "config.toml"), rewriteCodexArgs},
		{filepath.Join(home, ".config", "opencode", "opencode.json"), func(b []byte) []byte {
			return opencodeOldArgs.ReplaceAll(b, []byte(newArgsRepl))
		}},
	}
	var errs []error
	for _, t := range targets {
		if t.path == "" {
			continue
		}
		raw, err := os.ReadFile(t.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		next := t.rewrite(raw)
		if bytes.Equal(next, raw) {
			continue
		}
		verb := "would migrate"
		if !dryRun {
			if err := paths.AtomicWrite(t.path, next, filePerm(t.path)); err != nil {
				errs = append(errs, err)
				continue
			}
			verb = "migrated"
		}
		fmt.Fprintf(out, "web search  %s %s: web search -> network websearch\n", verb, t.path)
	}
	return errors.Join(errs...)
}

// rewriteCodexArgs rewrites args only inside [mcp_servers.*] tables whose command basename is claude-code.
func rewriteCodexArgs(raw []byte) []byte {
	return codexTable.ReplaceAllFunc(raw, func(tbl []byte) []byte {
		m := codexCommand.FindSubmatch(tbl)
		if m == nil || filepath.Base(string(m[1])) != "claude-code" {
			return tbl
		}
		return codexOldArgs.ReplaceAll(tbl, []byte(newArgsRepl))
	})
}

// isClaudeCodeCommand reports whether a server's "command" value (string, or argv array) is claude-code.
func isClaudeCodeCommand(cmd any) bool {
	var first string
	switch v := cmd.(type) {
	case string:
		first = v
	case []any:
		if len(v) == 0 {
			return false
		}
		first, _ = v[0].(string)
	default:
		return false
	}
	return first == "claude-code" || strings.HasSuffix(first, "/claude-code")
}

// claudeCodeWebSearchNames collects mcpServers names (top-level and every projects.*.mcpServers) whose
// command runs claude-code; byte-level rewrites below are scoped to these names only.
func claudeCodeWebSearchNames(doc map[string]any) map[string]bool {
	names := map[string]bool{}
	collect := func(servers map[string]any) {
		for name, raw := range servers {
			if entry, ok := raw.(map[string]any); ok && isClaudeCodeCommand(entry["command"]) {
				names[name] = true
			}
		}
	}
	if m, ok := doc["mcpServers"].(map[string]any); ok {
		collect(m)
	}
	if projects, ok := doc["projects"].(map[string]any); ok {
		for _, p := range projects {
			if proj, ok := p.(map[string]any); ok {
				if m, ok := proj["mcpServers"].(map[string]any); ok {
					collect(m)
				}
			}
		}
	}
	return names
}

// rewriteClaudeJSONArgs rewrites args ["web","search"] only for server names whose command runs claude-code,
// preserving the surrounding formatting via a per-name targeted byte match.
func rewriteClaudeJSONArgs(raw []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	out := raw
	for name := range claudeCodeWebSearchNames(doc) {
		pat := regexp.MustCompile(`("` + regexp.QuoteMeta(name) + `"\s*:\s*\{[^{}]*?"args"\s*:\s*\[\s*)"web"(\s*,\s*)"search"(\s*\])`)
		out = pat.ReplaceAll(out, []byte(newArgsRepl))
	}
	return out
}

// filePerm keeps the existing permission bits (these files can hold secrets).
func filePerm(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o600
}
