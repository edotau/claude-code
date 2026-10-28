// Package envscrub drops inherited credential/routing env from a child launch unless kept explicitly.
package envscrub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Deny is every inherited var a launch never passes through: another provider's or environment's
// credential/routing knob, which would otherwise override the launch's own resolution.
var Deny = []string{
	"DATABRICKS_*",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "AZURE_OPENAI_*",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"COPILOT_GITHUB_TOKEN", "COPILOT_PROVIDER_*",
	"HARNESS_*_API_KEY",
	// Vault-only pins and the session marker: an inherited one is never read, and a stale CLAUDE_PROFILE must not
	// outlive a parent shell (the launcher stamps its own after the scrub). Vendor-read *_MODEL names stay.
	"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL",
	"COPILOT_PROFILE", "CODEX_PROFILE", "GEMINI_PROFILE", "OPENCODE_PROFILE",
}

// KeepEnvVar opts bare shims back into an inherited name; their argv belongs to the vendor, not the harness.
const KeepEnvVar = "HARNESS_KEEP_ENV"

// VendorOnlyDeny drops from a vendor CLI child's env only; the Claude Code launch itself keeps these,
// since hooks inherit Claude's env (HARNESS_VAULT_KEY opens the vault there).
var VendorOnlyDeny = []string{"HARNESS_VAULT_KEY"}

// Scrub splits environ (KEY=VALUE entries) into kept and dropped (names only) against Deny, honoring keep.
func Scrub(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, Deny)
}

// ScrubVendor is Scrub plus VendorOnlyDeny: use for codex/gemini/opencode/copilot child launches.
func ScrubVendor(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, append(slices.Clone(Deny), VendorOnlyDeny...))
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

// KeepSet unions explicit names with HARNESS_KEEP_ENV, comma-separated.
func KeepSet(names []string) map[string]bool {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range strings.SplitSeq(os.Getenv(KeepEnvVar), ",") {
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
