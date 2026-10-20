package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
		paths.ProvidersOverlay(), // providers.json
		paths.ClaudeJSON(),       // ~/.claude.json
		paths.PinFile(),
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

// walkJSONStrings visits every string leaf of parsed JSON with its structural path.
func walkJSONStrings(prefix []string, node any, visit func(path []string, val string)) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			walkJSONStrings(append(prefix, k), child, visit)
		}
	case []any:
		for i, child := range v {
			walkJSONStrings(append(prefix, strconv.Itoa(i)), child, visit)
		}
	case string:
		visit(prefix, v)
	}
}

// Metadata paths skip only the generic heuristic, never known credential shapes.
func claudeMetadataPath(path []string) bool {
	switch len(path) {
	case 1:
		return claudeMetadataKey(path[0])
	case 2:
		switch path[0] {
		case "oauthAccount", "cachedArtifactRoster":
			return path[1] == "accountUuid" || path[1] == "organizationUuid"
		case "promoStartupStatusCache", "cachedUsageUtilization":
			return path[1] == "accountUuid"
		case "githubWebConnectionStatusCache":
			return path[1] == "accountUuid" || path[1] == "orgUuid"
		}
	case 3:
		switch path[0] {
		case "projects":
			return claudeMetadataKey(path[2])
		case "clientDataCacheSlots":
			return path[2] == "org"
		case "cachedExperimentData":
			return path[2] == "experimentId"
		}
	case 4:
		return path[0] == "projects" && path[2] == "activeWorktreeSession" &&
			(path[3] == "originalHeadCommit" || path[3] == "sessionId")
	}
	return false
}

func claudeMetadataKey(key string) bool {
	switch key {
	case "userID", "machineID", "remoteControlMachineId", "lastSessionId", "sessionId",
		"accountUuid", "organizationUuid", "orgUuid", "experimentId", "originalHeadCommit":
		return true
	}
	return false
}

func marketplaceRepoPath(path []string) bool {
	return len(path) == 4 && path[0] == "extraKnownMarketplaces" && path[2] == "source" && path[3] == "repo"
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
		walkJSONStrings(nil, doc, func(p []string, val string) {
			metadata := marketplaceRepoPath(p) ||
				(filepath.Base(path) == ".claude.json" && claudeMetadataPath(p))
			if looksLikeSecret(val, !metadata) {
				s.flag(fmt.Sprintf("%s → %s holds an inline secret", path, strings.Join(p, ".")))
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
