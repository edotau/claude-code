package copilot

import (
	"context"
	"errors"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/routing/models"
)

// SDKAsk is set by internal/driver/copilot/sdk at init; hook binaries leave it nil.
var SDKAsk func(context.Context, Options, string, []string) (Result, error)

// SDKListModels is set by internal/driver/copilot/sdk at init; hook binaries leave it nil.
var SDKListModels func(context.Context, Options) ([]Model, error)

var errNoSDK = errors.New("copilot: SDK runtime is not linked (import internal/driver/copilot/sdk)")

// Options configures one headless run. Env carries the COPILOT_PROVIDER_* BYOK block from Schema.Configure.
type Options struct {
	// OnUsage gets the cumulative usage after each model call, on the SDK goroutine
	OnUsage       func(wire.Usage)
	BudgetCredits *int
	// resolved copilot binary; empty is an error (never the SDK's bundled runtime)
	CLIPath string
	// KEY=VALUE BYOK block
	Env []string
	// names the credential scrub must not drop, from --keep-env
	KeepEnv []string
	// Copilot-catalog id; leave empty on BYOK (COPILOT_MODEL carries it)
	Model           string
	ReasoningEffort string
	EffortNotes     []string
	// ContextTier: "default" or "long_context"; empty leaves the model's own default
	Context          string
	WorkingDirectory string
	AllowAllTools    bool
	// always explicit: unset, the SDK silently falls back to the GitHub subscription
	UseLoggedInUser bool
	GitHubToken     string
	// MessageOptions.AgentMode = AgentModeAutopilot
	Autonomous bool
	// experimental session.RPC.Fleet.Start instead of SendAndWait
	Fleet bool
	// SessionConfig.Memory.Enabled
	EnableMemory bool
	// MsgMCP is the msg MCP server argv (binary first), loaded only on a tools run; empty loads no MCP.
	MsgMCP []string
	// non-empty resumes this session instead of creating a new one
	SessionID string
	// system text, sent as SystemMessage in append mode (replace would drop the SDK guardrails)
	System string
	// InstructionDirs, SkillDirs and AgentDirs are the discovery roots the harness renders; missing ones are skipped.
	InstructionDirs, SkillDirs, AgentDirs []string
	// InlineInstructions, when set, replaces discovery of every instruction file (see sameInstructions).
	InlineInstructions string
	// Providers+Models: the multi-provider route (Model = provider/id, Token feeds each BearerTokenProvider, no login).
	Providers []Provider
	Models    []RegistryModel
	Token     func() (string, error)
	// ChildEnv is set on, and ChildUnset (exact names) plus ChildUnsetPrefix stripped from, the registry child's env.
	ChildEnv, ChildUnset []string
	ChildUnsetPrefix     string
}

// Result is one completed turn; Model is what the runtime reports actually served it.
type Result struct {
	Text      string
	SessionID string
	Model     string
	// a tool request was approved, so the run may have side effects
	ToolRan   bool
	Usage     wire.Usage
	Transport *wire.Transport
	// FinishReason is the last model call's; ContentFiltered is set if any call was blocked or truncated by a filter.
	FinishReason    string
	ContentFiltered bool
	// Written is every path a successful built-in edit tool committed, from the tool-completion receipts
	Written []string
}

// Provider is one named upstream: Type anthropic|openai, WireAPI "" (anthropic), completions|responses.
type Provider struct {
	Name    string
	Type    string
	WireAPI string
	BaseURL string
	Headers map[string]string
}

// RegistryModel is one selectable entry; the CLI selects it as Provider + "/" + ID.
type RegistryModel struct {
	ID       string
	Provider string
	// the Databricks endpoint id
	WireModel string
	// copilot's catalog form, so its per-model settings apply
	ModelID                string
	Name                   string
	MaxPromptTokens        int
	MaxContextWindowTokens int
	MaxOutputTokens        int
}

// Ref is the CLI's selection string for m.
func (m RegistryModel) Ref() string { return m.Provider + "/" + m.ID }

// WellKnownModel maps a gateway Claude id to copilot's form (claude-sonnet-5.5) so Claude settings apply; else as is.
func WellKnownModel(id string) string {
	canonical, _, ok := models.Resolve(id)
	if !ok || models.FamilyKind(canonical) != models.FamilyClaude {
		return id
	}
	return models.Wire("dotted", canonical)
}

// Model is the catalog entry the harness prints and validates pins against.
type Model struct {
	ID              string
	Name            string
	Vision          bool
	ReasoningEffort bool
	// nil when the model carries no premium billing multiplier
	Multiplier *float64
	// SupportedReasoningEfforts; empty when the model has none
	Efforts []string
	// "" when the catalog names none
	DefaultEffort string
	// MaxContextWindowTokens; 0 when the catalog reports none
	ContextTokens int
	// Policy == nil or Policy.State == "enabled"
	Enabled bool
}

// CatalogOptions is a listing's auth: a resolved account token (catalogAuth), else "" for the CLI's own login.
func CatalogOptions(cliPath, token string) Options {
	if token != "" {
		return Options{CLIPath: cliPath, GitHubToken: token}
	}
	return Options{CLIPath: cliPath, UseLoggedInUser: true}
}

// MinMultiProviderCLI is the oldest copilot CLI proven to serve SDK named providers (probed 2026-10-28).
const MinMultiProviderCLI = "1.0.104"

// MultiProvider reports whether the run uses the named-provider registry.
func (o Options) MultiProvider() bool { return len(o.Providers) > 0 || len(o.Models) > 0 }
