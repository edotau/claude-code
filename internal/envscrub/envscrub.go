// Package envscrub drops inherited credential/routing env from a child launch unless kept explicitly.
package envscrub

import (
	"os"
	"path/filepath"
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
}

// KeepEnvVar opts bare shims back into an inherited name; their argv belongs to the vendor, not the harness.
const KeepEnvVar = "HARNESS_KEEP_ENV"

// Scrub splits environ (KEY=VALUE entries) into kept and dropped (names only) against Deny, honoring keep.
func Scrub(environ []string, keep map[string]bool) (kept, dropped []string) {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep[name] || !denied(name) {
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

func denied(name string) bool {
	for _, pat := range Deny {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}
