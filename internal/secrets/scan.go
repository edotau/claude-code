package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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

// DefaultScanTargets lists settings.json, settings.local.json, ~/.claude.json and .mcp.json for a scan with no paths.
func DefaultScanTargets() []string {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	out := []string{
		filepath.Join(paths.ConfigDir(), "settings.json"),
		filepath.Join(paths.ConfigDir(), "settings.local.json"),
		paths.ClaudeJSON(), // ~/.claude.json
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
		case "chromeExtension":
			return path[1] == "pairedDeviceId"
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

var envPairName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// envPairValue returns the value of a NAME=value launch-env entry, so the name alone never reads as a secret.
func envPairValue(val string) string {
	if loc := envPairName.FindStringIndex(val); loc != nil {
		return val[loc[1]:]
	}
	return val
}

func marketplaceRepoPath(path []string) bool {
	return len(path) == 4 && path[0] == "extraKnownMarketplaces" && path[2] == "source" && path[3] == "repo"
}

// ScanFile inspects JSON leaves or KEY=value lines in an explicitly supplied file; missing files are silently fine.
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

// ScanVault inspects the vault's unencrypted buckets (provider overlay, pins, custom env) for inline secrets.
func (s *Scanner) ScanVault() {
	var found []string
	err := vault.View(func(t *vault.Tx) error {
		for _, key := range []string{"providers", "providers.prev", "vendors"} {
			raw, ok := t.Get(vault.Overlay, key)
			if !ok {
				continue
			}
			var doc any
			if json.Unmarshal([]byte(raw), &doc) == nil {
				walkJSONStrings(nil, doc, func(p []string, val string) {
					if looksLikeSecret(envPairValue(val), true) {
						found = append(found, "vault:overlay/"+key+" → "+strings.Join(p, ".")+" holds an inline secret")
					}
				})
			}
		}
		for _, bucket := range []string{vault.Pins, vault.Env} {
			kv := t.All(bucket)
			keys := make([]string, 0, len(kv))
			for k := range kv {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if v := kv[k]; v != providers.SecretRef && LooksLikeSecret(v) {
					found = append(found, "vault:"+bucket+" → "+k+" holds an inline secret")
				}
			}
		}
		return nil
	})
	if err != nil {
		s.flag(fmt.Sprintf("vault — could not read: %v", err))
		return
	}
	for _, f := range found {
		s.flag(f)
	}
	if len(found) == 0 {
		s.ok("vault — clean")
	}
}

// ScanConfigs audits targets (DefaultScanTargets plus the vault when empty) and prints the summary line.
func (s *Scanner) ScanConfigs(targets []string) int {
	defaults := len(targets) == 0
	if defaults {
		targets = DefaultScanTargets()
	}
	fmt.Fprintln(s.out(), "== config-surface credential scan ==")
	for _, f := range targets {
		s.ScanFile(f)
	}
	if defaults {
		s.ScanVault()
	}
	if n := len(s.Findings); n > 0 {
		fmt.Fprintf(s.out(), "secrets scan: %d finding(s) — move inline values into the vault with `claude-code secrets set`.\n", n)
		return ExitFindings
	}
	fmt.Fprintln(s.out(), "secrets scan: clean.")
	return ExitClean
}
