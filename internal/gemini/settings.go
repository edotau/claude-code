package gemini

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
)

// renderGeminiSettings merges the spec's exact-match guard denies into agy's
// ~/.gemini/antigravity-cli/settings.json permissions.deny, preserving every other key.
func renderGeminiSettings(home string, o render.RenderOptions, out io.Writer) (bool, error) {
	cfg, err := agentconfig.LoadConfig()
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: load agentconfig: %w", err)
	}
	if !cfg.Agent("gemini").Permission.UseGuardRules {
		return false, nil
	}
	rules := agyGuardRules(cfg.Guard().Deny, out)
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	return render.CommitJSONObject(path, o, out, "gemini-settings", func(obj map[string]any, existed bool) bool {
		if len(rules) == 0 && !existed {
			return false // nothing to deny: don't create agy's settings.json just to write an empty list
		}
		perms, _ := obj["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		existing, _ := perms["deny"].([]any)
		perms["deny"] = mergeAgyDeny(existing, rules)
		obj["permissions"] = perms
		return true
	})
}

// agyGuardRules converts exact-match Bash(...) denies to agy command(...) rules, noting the rest.
func agyGuardRules(deny []string, out io.Writer) []string {
	var rules []string
	for _, r := range deny {
		cmd, ok := strings.CutPrefix(r, "Bash(")
		cmd, ok2 := strings.CutSuffix(cmd, ")")
		if !ok || !ok2 || strings.HasSuffix(cmd, ":*") {
			fmt.Fprintf(out, "gemini-settings: note: %q is not exact-match; agy denies exact commands only, skipped\n", r)
			continue
		}
		rules = append(rules, "command("+cmd+")")
	}
	return rules
}

// mergeAgyDeny returns the string entries of existing, then rules, without duplicates.
func mergeAgyDeny(existing []any, rules []string) []string {
	seen := map[string]bool{}
	merged := make([]string, 0, len(existing)+len(rules))
	for _, e := range existing {
		s, ok := e.(string)
		if !ok || seen[s] {
			continue
		}
		seen[s] = true
		merged = append(merged, s)
	}
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		merged = append(merged, r)
	}
	return merged
}
