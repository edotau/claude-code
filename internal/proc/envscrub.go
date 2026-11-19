// Env scrubbing: drops inherited credential/routing env from a child launch unless kept explicitly.

package proc

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Shared launch environment names keep overlay wiring and inherited-env scrubbing aligned.
const (
	CopilotProviderPrefix             = "COPILOT_PROVIDER_"
	EnvCopilotProvidersConfig         = "COPILOT_PROVIDERS_CONFIG"
	EnvClaudeConfigDir                = "CLAUDE_CONFIG_DIR"
	EnvAnthropicAPIKey                = "ANTHROPIC_API_KEY"
	envAnthropicAuthToken             = "ANTHROPIC_AUTH_TOKEN"
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
	EnvAnthropicAPIKey, envAnthropicAuthToken, EnvAnthropicBaseURL, EnvAnthropicCustomHeaders,
}

// ScrubDeny is every inherited credential/routing var a launch never passes through (it would override its own).
var ScrubDeny = append(slices.Clone(AnthropicStripEnv), []string{
	"DATABRICKS_*", "AGY_LLM_GATEWAY_*",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "AZURE_OPENAI_*",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"COPILOT_GITHUB_TOKEN", CopilotProviderPrefix + "*", EnvCopilotProvidersConfig,
	"HARNESS_*_API_KEY",
	// Vault-only pins and the session marker: never read from the inherited env; agent-read *_MODEL names stay.
	"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL",
	"COPILOT_PROFILE", "CODEX_PROFILE", "GEMINI_PROFILE", "OPENCODE_PROFILE",
	"HARNESS_CODEX_PROFILE", "HARNESS_CODEX_CATALOG",
}...)

// ScrubDenyCredentials is ScrubDeny plus every credential-shaped name glob, for a child of unknown credential owner.
var ScrubDenyCredentials = append(slices.Clone(ScrubDeny), "*_KEY", "*_TOKEN", "*_SECRET", "*_PASSWORD", "*_CREDENTIALS")

// OwnLoginEnv is each agent's own sign-in credential env by agent name; a Databricks overlay hop holds none of them.
var OwnLoginEnv = map[string][]string{
	"claude": {"CLAUDE_CODE_OAUTH_TOKEN", EnvAnthropicAPIKey}, // ANTHROPIC_AUTH_TOKEN is overlay wiring, not a login
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

// envKeepVar opts bare shims back into an inherited name; their argv belongs to the agent, not the harness.
const envKeepVar = "HARNESS_KEEP_ENV"

// scrubAgentOnlyDeny drops from an agent CLI child only; Claude Code keeps these so its hooks can open the vault.
var scrubAgentOnlyDeny = []string{"HARNESS_VAULT_KEY"}

// Scrub splits environ (KEY=VALUE entries) into kept and dropped (names only) against ScrubDeny, honoring keep.
func Scrub(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, scrubGlobs())
}

// ScrubAgent is Scrub plus scrubAgentOnlyDeny: use for codex/gemini/opencode/copilot child launches.
func ScrubAgent(environ []string, keep map[string]bool) (kept, dropped []string) {
	return scrub(environ, keep, scrubAgentGlobs())
}

// ScrubAgentCredentials is ScrubAgent plus every credential-shaped name except keep and agent's OwnLoginEnv.
func ScrubAgentCredentials(environ []string, keep map[string]bool, agent string) (kept, dropped []string) {
	kept, dropped = ScrubAgent(environ, keep)
	spared := maps.Clone(keep)
	if spared == nil {
		spared = map[string]bool{}
	}
	for _, n := range OwnLoginEnv[agent] {
		spared[n] = true
	}
	kept, more := scrub(kept, spared, scrubCredentialGlobs())
	return kept, append(dropped, more...)
}

// Drop returns environ without the names matching a deny glob, except those in keep.
func Drop(environ []string, keep map[string]bool, deny []string) []string {
	kept, _ := scrub(environ, keep, compileDeny(deny))
	return kept
}

var (
	scrubGlobs           = sync.OnceValue(func() []glob { return compileDeny(ScrubDeny) })
	scrubAgentGlobs      = sync.OnceValue(func() []glob { return compileDeny(slices.Concat(ScrubDeny, scrubAgentOnlyDeny)) })
	scrubCredentialGlobs = sync.OnceValue(func() []glob { return compileDeny(ScrubDenyCredentials) })
)

func scrub(environ []string, keep map[string]bool, globs []glob) (kept, dropped []string) {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep[name] || !slices.ContainsFunc(globs, func(g glob) bool { return g.match(name) }) {
			kept = append(kept, kv)
			continue
		}
		dropped = append(dropped, name)
	}
	return kept, dropped
}

// EnvBlocked reports whether a launch never passes name through (ScrubDeny, scrubAgentOnlyDeny, HARNESS_*).
func EnvBlocked(name string) bool {
	return strings.HasPrefix(name, "HARNESS_") || slices.ContainsFunc(scrubAgentGlobs(), func(g glob) bool { return g.match(name) })
}

// EnvKeepSet unions explicit names with HARNESS_KEEP_ENV, comma-separated.
func EnvKeepSet(names []string) map[string]bool {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range strings.SplitSeq(os.Getenv(envKeepVar), ",") {
		if n = strings.TrimSpace(n); n != "" {
			keep[n] = true
		}
	}
	return keep
}

// glob is one deny pattern classified once: a literal, prefix*, *suffix, or anything else (filepath.Match).
type glob struct {
	pat, lit string
	kind     byte // 'l' literal, 'p' prefix, 's' suffix, 'm' filepath.Match
}

func compileDeny(deny []string) []glob {
	out := make([]glob, len(deny))
	for i, pat := range deny {
		j := strings.IndexAny(pat, `*?[\`)
		switch {
		case j < 0:
			out[i] = glob{pat: pat, lit: pat, kind: 'l'}
		case j == len(pat)-1 && pat[j] == '*':
			out[i] = glob{pat: pat, lit: pat[:j], kind: 'p'}
		case j == 0 && pat[0] == '*' && !strings.ContainsAny(pat[1:], `*?[\`):
			out[i] = glob{pat: pat, lit: pat[1:], kind: 's'}
		default:
			out[i] = glob{pat: pat, kind: 'm'}
		}
	}
	return out
}

// match is filepath.Match(g.pat, name) with the common shapes decided without the matcher.
func (g glob) match(name string) bool {
	switch g.kind {
	case 'l':
		return name == g.lit
	case 'p':
		return strings.HasPrefix(name, g.lit) && !strings.Contains(name[len(g.lit):], "/")
	case 's':
		return strings.HasSuffix(name, g.lit) && !strings.Contains(name[:len(name)-len(g.lit)], "/")
	}
	ok, _ := filepath.Match(g.pat, name)
	return ok
}
