// Env scrubbing: drops inherited credential/routing env from a child launch unless kept explicitly.

package proc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ScrubDeny is every inherited var a launch never passes through: another provider's or environment's
// credential/routing knob, which would otherwise override the launch's own resolution.
var ScrubDeny = []string{
	"DATABRICKS_*",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "AZURE_OPENAI_*",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"COPILOT_GITHUB_TOKEN", "COPILOT_PROVIDER_*", "COPILOT_PROVIDERS_CONFIG",
	"HARNESS_*_API_KEY",
	// Vault-only pins and the session marker: an inherited one is never read, and a stale CLAUDE_PROFILE must not
	// outlive a parent shell (the launcher stamps its own after the scrub). Agent-read *_MODEL names stay.
	"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL",
	"COPILOT_PROFILE", "CODEX_PROFILE", "GEMINI_PROFILE", "OPENCODE_PROFILE",
	"HARNESS_CODEX_PROFILE", "HARNESS_CODEX_CATALOG",
}

// EnvKeepVar opts bare shims back into an inherited name; their argv belongs to the agent, not the harness.
const EnvKeepVar = "HARNESS_KEEP_ENV"

// ScrubAgentOnlyDeny drops from an agent CLI child's env only; the Claude Code launch itself keeps these,
// since hooks inherit Claude's env (HARNESS_VAULT_KEY opens the vault there).
var ScrubAgentOnlyDeny = []string{"HARNESS_VAULT_KEY"}

// Scrub splits environ (KEY=VALUE entries) into kept and dropped (names only) against ScrubDeny, honoring keep.
func Scrub(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, ScrubDeny)
}

// ScrubAgent is Scrub plus ScrubAgentOnlyDeny: use for codex/gemini/opencode/copilot child launches.
func ScrubAgent(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, append(slices.Clone(ScrubDeny), ScrubAgentOnlyDeny...))
}

func scrub(environ []string, keep map[string]bool, deny []string) (kept, dropped []string) {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep[name] || !denied(name, deny) {
			kept = append(kept, kv)
			continue
		}
		dropped = append(dropped, name)
	}
	return kept, dropped
}

// EnvBlocked reports whether name is one a launch never passes through (ScrubDeny, ScrubAgentOnlyDeny, anything HARNESS_*).
func EnvBlocked(name string) bool {
	return strings.HasPrefix(name, "HARNESS_") || denied(name, ScrubDeny) || denied(name, ScrubAgentOnlyDeny)
}

// EnvKeepSet unions explicit names with HARNESS_KEEP_ENV, comma-separated.
func EnvKeepSet(names []string) map[string]bool {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range strings.SplitSeq(os.Getenv(EnvKeepVar), ",") {
		if n = strings.TrimSpace(n); n != "" {
			keep[n] = true
		}
	}
	return keep
}

func denied(name string, deny []string) bool {
	for _, pat := range deny {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}
