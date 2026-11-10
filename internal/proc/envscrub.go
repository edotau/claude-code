// Env scrubbing: drops inherited credential/routing env from a child launch unless kept explicitly.

package proc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Shared launch environment names keep route wiring and inherited-env scrubbing aligned.
const (
	CopilotProviderPrefix             = "COPILOT_PROVIDER_"
	EnvCopilotProvidersConfig         = "COPILOT_PROVIDERS_CONFIG"
	EnvClaudeConfigDir                = "CLAUDE_CONFIG_DIR"
	EnvAnthropicAPIKey                = "ANTHROPIC_API_KEY"
	EnvAnthropicAuthToken             = "ANTHROPIC_AUTH_TOKEN"
	EnvAnthropicBaseURL               = "ANTHROPIC_BASE_URL"
	EnvAnthropicCustomHeaders         = "ANTHROPIC_CUSTOM_HEADERS"
	EnvCopilotProviderBaseURL         = CopilotProviderPrefix + "BASE_URL"
	EnvCopilotProviderType            = CopilotProviderPrefix + "TYPE"
	EnvCopilotProviderAPIKey          = CopilotProviderPrefix + "API_KEY"
	EnvCopilotProviderBearerToken     = CopilotProviderPrefix + "BEARER_TOKEN"
	EnvCopilotProviderAPIKeyCommand   = CopilotProviderPrefix + "API_KEY_COMMAND"
	EnvCopilotProviderWireAPI         = CopilotProviderPrefix + "WIRE_API"
	EnvCopilotProviderModelID         = CopilotProviderPrefix + "MODEL_ID"
	EnvCopilotProviderMaxPromptTokens = CopilotProviderPrefix + "MAX_PROMPT_TOKENS"
	EnvCopilotProviderMaxOutputTokens = CopilotProviderPrefix + "MAX_OUTPUT_TOKENS"
	EnvCopilotProviderHeaders         = CopilotProviderPrefix + "HEADERS"
)

// AnthropicStripEnv names inherited endpoint credentials shared by all launch scrubbers.
var AnthropicStripEnv = []string{
	EnvAnthropicAPIKey, EnvAnthropicAuthToken, EnvAnthropicBaseURL, EnvAnthropicCustomHeaders,
}

// ScrubDeny is every inherited credential/routing var a launch never passes through (it would override its own).
var ScrubDeny = append(slices.Clone(AnthropicStripEnv), []string{
	"DATABRICKS_*",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "AZURE_OPENAI_*",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"COPILOT_GITHUB_TOKEN", CopilotProviderPrefix + "*", EnvCopilotProvidersConfig,
	"HARNESS_*_API_KEY",
	// Vault-only pins and the session marker: never read from the inherited env; agent-read *_MODEL names stay.
	"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL",
	"COPILOT_PROFILE", "CODEX_PROFILE", "GEMINI_PROFILE", "OPENCODE_PROFILE",
	"HARNESS_CODEX_PROFILE", "HARNESS_CODEX_CATALOG",
}...)

// OwnLoginEnv is each agent's own sign-in credential env by agent name; a Databricks route hop holds none of them.
var OwnLoginEnv = map[string][]string{
	"claude": {"CLAUDE_CODE_OAUTH_TOKEN", EnvAnthropicAPIKey}, // ANTHROPIC_AUTH_TOKEN is route wiring, not a login
	"codex":  {"OPENAI_API_KEY"},
	"copilot": {"COPILOT_GITHUB_TOKEN", "COPILOT_SDK_AUTH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN", "GH_ENTERPRISE_TOKEN"},
	"gemini":   {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
	"opencode": {"OPENCODE_API_KEY"},
}

// ForeignLogins is every agent's OwnLoginEnv except own's; keepGHToken spares GH_TOKEN, which git and gh need.
func ForeignLogins(own string, keepGHToken bool) []string {
	var out []string
	for name, envs := range OwnLoginEnv {
		if name != own {
			out = append(out, slices.DeleteFunc(slices.Clone(envs), func(n string) bool {
				return keepGHToken && n == "GH_TOKEN"
			})...)
		}
	}
	return out
}

// EnvKeepVar opts bare shims back into an inherited name; their argv belongs to the agent, not the harness.
const EnvKeepVar = "HARNESS_KEEP_ENV"

// ScrubAgentOnlyDeny drops from an agent CLI child only; Claude Code keeps these so its hooks can open the vault.
var ScrubAgentOnlyDeny = []string{"HARNESS_VAULT_KEY"}

// Scrub splits environ (KEY=VALUE entries) into kept and dropped (names only) against ScrubDeny, honoring keep.
func Scrub(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, ScrubDeny)
}

// ScrubAgent is Scrub plus ScrubAgentOnlyDeny: use for codex/gemini/opencode/copilot child launches.
func ScrubAgent(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, append(slices.Clone(ScrubDeny), ScrubAgentOnlyDeny...))
}

// Drop returns environ without the names matching a deny glob, except those in keep.
func Drop(environ []string, keep map[string]bool, deny []string) []string {
	kept, _ := scrub(environ, keep, deny)
	return kept
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
