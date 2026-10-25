// Package providers is the model-routing registry: named upstreams, their gateway routes per wire
// dialect, credential sources, and the tier→model pins a launch or routed request resolves against.
package providers

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// Wire dialects a provider route can speak.
const (
	DialectAnthropic = "anthropic" // POST {route}/v1/messages
	DialectOpenAI    = "openai"    // POST {route}/chat/completions
	DialectResponses = "responses" // POST {route}/responses
	DialectGemini    = "gemini"    // Google-native generateContent
)

// Auth header styles.
const (
	AuthAPIKey      = "x-api-key"   // Anthropic-native key header
	AuthBearer      = "bearer"      // Authorization: Bearer <cred>
	AuthNone        = "none"        // local servers (ollama, llama.cpp)
	AuthPassthrough = "passthrough" // forward the client's own auth (Claude subscription OAuth)
)

// Provider is one upstream: a direct API or a gateway exposing several dialects under one host.
type Provider struct {
	Name string `json:"-"`
	// Kind is the dialect BaseURL itself speaks; Routes add further dialects (gateway paths).
	Kind    string            `json:"kind"`
	BaseURL string            `json:"base_url"`
	Routes  map[string]string `json:"routes,omitempty"` // dialect → path suffix or absolute URL
	Auth    Auth              `json:"auth"`
	Models  map[string]string `json:"models,omitempty"` // slot (opus/sonnet/haiku/fable) → model id
	Headers map[string]string `json:"headers,omitempty"`
	// OneM opts the provider's Claude ids into the [1m] marker; only true where the upstream serves 1M context.
	OneM bool `json:"one_m,omitempty"`
	// OpencodeFallback is the model id opencode's own render substitutes whenever a slot resolves to
	// "auto" (a router alias some CLIs serve but opencode's provider catalog does not list).
	OpencodeFallback string `json:"opencode_fallback,omitempty"`
	// OpencodeVariant, if set, is appended as "#<variant>" to the rendered opencode.json default model.
	OpencodeVariant string `json:"opencode_variant,omitempty"`
	// OpencodeID is this provider's key in opencode's own auth.json/provider catalog (e.g. "openai",
	// "github-copilot", "google", "nvidia"); empty means the opencode hub cannot route to it.
	OpencodeID string `json:"opencodeID,omitempty"`
	// Preset names an expansion ("databricks") that derives kind/base_url/routes/auth/models/env at load.
	Preset string `json:"preset,omitempty"`
	// Host is the one workspace URL a preset derives from (https, no path).
	Host string `json:"host,omitempty"`
	// CLIProfile is the ~/.databrickscfg profile a databricks preset's auth commands use.
	CLIProfile string `json:"cliProfile,omitempty"`
	// Env is the non-secret launch env for sessions on this provider.
	Env map[string]string `json:"env,omitempty"`
	// ModelPrefix decorates canonical model ids to this provider's wire ids (databricks preset: "databricks-").
	ModelPrefix string `json:"modelPrefix,omitempty"`
	// Options are a preset row's non-secret knobs (databricks: options.wire serving|gateway).
	Options Options `json:"options,omitzero"`
}

const oneMMarker = "[1m]"

// canonicalFamilies are the first-party id families WireModel decorates.
var canonicalFamilies = []string{"claude-", "gpt-", "gemini-"}

// WireModel decorates a canonical model id to p's wire id; idempotent, keeps a trailing [1m], leaves qualified ids.
func (p Provider) WireModel(id string) string {
	base, oneM := strings.CutSuffix(id, oneMMarker)
	if base == "" || p.ModelPrefix == "" || strings.HasPrefix(base, p.ModelPrefix) || strings.ContainsAny(base, "/:") {
		return id
	}
	// Only first-party canonical families are decorated; a custom endpoint name stays exactly as written.
	if !slices.ContainsFunc(canonicalFamilies, func(f string) bool { return strings.HasPrefix(base, f) }) {
		return id
	}
	base = p.ModelPrefix + base
	base = p.presetWireID(base)
	if oneM {
		base += oneMMarker
	}
	return base
}

// CanonicalModel is the inverse of WireModel: it strips p's prefix and keeps a trailing [1m].
func (p Provider) CanonicalModel(id string) string {
	if p.ModelPrefix == "" {
		return id
	}
	base, oneM := strings.CutSuffix(id, oneMMarker)
	base = strings.TrimPrefix(base, p.ModelPrefix)
	if oneM {
		base += oneMMarker
	}
	return base
}

// MarshalJSON redacts at the type: an inline gateway token in Headers never reaches a JSON dump, and
// has_secret_headers says one exists. A writer that wants the raw map must not go through Marshal.
func (p Provider) MarshalJSON() ([]byte, error) {
	type plain Provider // no MarshalJSON, so the alias cannot recurse
	v := plain(p)
	v.Headers = shapes.RedactMap(p.Headers)
	return json.Marshal(struct {
		plain
		HasSecretHeaders bool `json:"has_secret_headers,omitempty"`
	}{v, shapes.Any(p.Headers)})
}

// Auth names where the credential comes from; the first non-empty source wins (see Credential).
type Auth struct {
	Type    string `json:"type"`              // x-api-key | bearer | none | passthrough
	Env     string `json:"env,omitempty"`     // env var, also read from the vault
	File    string `json:"file,omitempty"`    // file holding the credential (~ expanded)
	Command string `json:"command,omitempty"` // shell command printing the credential (keychain, op, OAuth mint)
	TTL     int    `json:"ttl,omitempty"`     // seconds a Command result may be cached; 0 = 300
	Login   string `json:"login,omitempty"`   // passthrough only: vendor CLI whose own sign-in serves it (codex, copilot)
	// RefreshCommand forces a fresh credential; run when Command's token is near expiry or on a 401.
	RefreshCommand string `json:"refreshCommand,omitempty"`
	// AuthType picks the mint path (Databricks SDK names): databricks-cli | oauth-m2m | pat; "" keeps env/file/command.
	AuthType string `json:"authType,omitempty"`
	ClientID string `json:"clientId,omitempty"` // oauth-m2m service-principal client id (not secret)
	// AccountID and WorkspaceID are non-secret Databricks ids; stored and shown in --list json, no behavior uses them yet.
	AccountID   string `json:"accountId,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

// APIKeyAuth reports whether p bills per token on a harness-held key, not a subscription login (ledger gate).
func (p Provider) APIKeyAuth() bool {
	return p.Auth.Type == AuthAPIKey || p.Auth.Type == AuthBearer
}

// Route returns the base URL serving dialect: an explicit route (absolute, or a path joined onto
// BaseURL), else BaseURL itself when dialect == Kind.
func (p Provider) Route(dialect string) (string, bool) {
	if r, ok := p.Routes[dialect]; ok && r != "" {
		if u, err := url.Parse(r); err == nil && u.IsAbs() {
			return strings.TrimRight(r, "/"), true
		}
		return strings.TrimRight(p.BaseURL, "/") + "/" + strings.Trim(r, "/"), true
	}
	if dialect == p.Kind && p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/"), true
	}
	return "", false
}

// Dialects lists every dialect the provider can serve, sorted.
func (p Provider) Dialects() []string {
	seen := map[string]bool{}
	if p.Kind != "" && p.BaseURL != "" {
		seen[p.Kind] = true
	}
	for d, r := range p.Routes {
		if r != "" {
			seen[d] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// SlotOf reverse-maps a model id to the slot it occupies in this provider, for cross-provider failover.
func (p Provider) SlotOf(model string) string {
	for _, slot := range models.Slots {
		if id, ok := p.Models[slot]; ok && id == model {
			return slot
		}
	}
	return ""
}

// Registry is the merged provider table plus the default selection and failover ladder.
type Registry struct {
	Default   string               `json:"default"`
	Fallback  []string             `json:"fallback,omitempty"`
	Providers map[string]*Provider `json:"providers"`
	// OpencodePlugins renders opencode's own `plugin` config key; empty (the default) leaves it untouched.
	OpencodePlugins []string `json:"opencodePlugins,omitempty"`
	// AgentPlans names each agent's sign-in plans for configure's mode column, keyed by agent name.
	AgentPlans map[string][]Plan `json:"agentPlans,omitempty"`
	// AgentPlanFormat renders a plan's mode column from {provider} {mode} {tier} {id}; empty = lowercase default.
	AgentPlanFormat string `json:"agentPlanFormat,omitempty"`
	// Routing is the one routing object: weights, cell overrides, subagent offload and opencode's sub-pick
	// knobs (was top-level route, routeWeights, opencodeLadder — internal/providers/routing.go).
	Routing Routing `json:"routing,omitzero"`
	// Profiles is the per-agent account/model/effort table (profiles.go); the zero value changes nothing.
	Profiles Profiles `json:"profiles,omitzero"`
}

// RouteCell is one overlay cell for internal/route's table.
type RouteCell struct {
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Account string `json:"account_key,omitempty"`
}

// RouteRunners are the routing table's columns, in roster order; internal/routetable embeds the same list.
var RouteRunners = []string{"claude", "copilot", "codex", "opencode", "gemini"}

// RouteTiers are the routed tiers, strongest first; fable stays Claude Code's own slot.
var RouteTiers = []string{models.Opus, models.Sonnet, models.Haiku}

// RouteWeightNames are the dimension names a route weights map must sum to 1 over (internal/route's
// Dimension.Name(); today "price", "headroom"); this package only validates shape, route owns the scoring.
var RouteWeightNames = []string{"price", "headroom"}

// RouteBillingKeys are the routing table's static account_key values (internal/routetable's own Billing
// map mirrors this list, since routetable can't import providers' validator back); a "copilot:" prefix
// also passes an overlay cell's account_key, since routetable adds the pinned gh user's key
// (copilot:<user>, or CopilotUnpinned = "copilot:unpinned") dynamically at load, before any pin is known
// here.
var RouteBillingKeys = []string{"claude:max", "codex:chatgpt", "gemini:antigravity", "opencode:go",
	"opencode:github-copilot", "opencode:openai", "opencode:nvidia", "opencode:zen-free"}

// RouteOpencodeModelPrefixes are the model prefixes an opencode override cell may carry without an explicit
// account_key: opencode-go/opencode keep the table's own opencode:go account, github-copilot bills
// opencode's own Copilot login (opencode:github-copilot), openai/nvidia bill their own opencode logins —
// kept in sync with internal/routetable's opencodeOverrideAccount, which owns the actual account-key mapping.
var RouteOpencodeModelPrefixes = []string{"opencode-go", "opencode", "github-copilot", "openai", "nvidia"}

// RouteOpencodeSubIDs are the ids an opencodeLadder entry may name: opencode's own plan (opencode-go) has
// no registry provider, every other id matches some provider's opencodeID (github-copilot, openai, nvidia).
// zen-free is not here — it always runs first and is never ladder-ordered. Kept in sync with
// internal/routetable's table.json opencode_sub keys (minus zen-free).
var RouteOpencodeSubIDs = []string{"opencode-go", "github-copilot", "openai", "nvidia"}

// validRouteAccountKey reports whether key is a known static billing key or the "copilot:" dynamic family.
func validRouteAccountKey(key string) bool {
	return strings.HasPrefix(key, "copilot:") || slices.Contains(RouteBillingKeys, key)
}

// routeModelPrefix returns the provider prefix of a "prefix/model" id ("" when model carries no prefix).
func routeModelPrefix(model string) string {
	prefix, _, ok := strings.Cut(model, "/")
	if !ok {
		return ""
	}
	return prefix
}

// ValidateRouteWeights requires one weight per RouteWeightNames entry, each in [0,1], summing to 1; an
// empty map is skipped (an overlay that does not touch weights leaves the table's own weights alone).
func ValidateRouteWeights(weights map[string]float64) error {
	if len(weights) == 0 {
		return nil
	}
	sum := 0.0
	for _, name := range RouteWeightNames {
		w, ok := weights[name]
		if !ok || w < 0 || w > 1 {
			return fmt.Errorf("route weight %q missing or outside [0,1]", name)
		}
		sum += w
	}
	if len(weights) != len(RouteWeightNames) {
		return fmt.Errorf("route weights name %d dimensions, want %d", len(weights), len(RouteWeightNames))
	}
	if math.Abs(sum-1) > 1e-9 {
		return fmt.Errorf("route weights sum to %g, want 1", sum)
	}
	return nil
}

// Plan is one sign-in plan: ID matches the raw plan ID the login reports (* globs); Tier and Provider override
// the detected plan name and the agent's vendor.
type Plan struct {
	ID       string `json:"id"`
	Tier     string `json:"tier,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// Get returns the named provider or an error naming the known ones.
func (r *Registry) Get(name string) (*Provider, error) {
	if p, ok := r.Providers[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(r.Names(), ", "))
}

// Names lists provider names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.Providers))
	for n := range r.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Validate reports registry errors: unknown dialects, missing base URLs, dangling default/fallback names.
func (r *Registry) Validate() error {
	var errs []string
	valid := map[string]bool{DialectAnthropic: true, DialectOpenAI: true, DialectResponses: true, DialectGemini: true}
	for _, n := range r.Names() {
		errs = append(errs, validateProvider(n, r.Providers[n], valid)...)
	}
	errs = append(errs, validateSecretNames(r)...)
	errs = append(errs, validateNameRefs(r)...)
	errs = append(errs, validateRouting(r.Routing)...)
	errs = append(errs, validateProfiles(r)...)
	if len(errs) > 0 {
		return fmt.Errorf("providers: %s", strings.Join(errs, "; "))
	}
	return nil
}

// validateSecretNames rejects two sealed-authType providers whose derived secret names collide.
func validateSecretNames(r *Registry) []string {
	var errs []string
	owner := map[string]string{}
	for _, n := range r.Names() {
		name := authSecretName(r.Providers[n])
		if name == "" {
			continue
		}
		if o, ok := owner[name]; ok {
			errs = append(errs, fmt.Sprintf("%s and %s would share sealed secret %s; rename one", o, n, name))
			continue
		}
		owner[name] = n
	}
	return errs
}

// validateProvider checks one provider's dialect, routes, and auth shape.
func validateProvider(n string, p *Provider, valid map[string]bool) []string {
	var errs []string
	if !valid[p.Kind] {
		errs = append(errs, fmt.Sprintf("%s: kind %q is not a dialect", n, p.Kind))
	}
	if p.BaseURL == "" && len(p.Routes) == 0 {
		errs = append(errs, n+": needs base_url or routes")
	}
	for d := range p.Routes {
		if !valid[d] {
			errs = append(errs, fmt.Sprintf("%s: route dialect %q unknown", n, d))
		}
	}
	switch p.Auth.Type {
	case AuthAPIKey, AuthBearer, AuthNone, AuthPassthrough:
	default:
		errs = append(errs, fmt.Sprintf("%s: auth.type %q unknown", n, p.Auth.Type))
	}
	if p.Auth.Login != "" && p.Auth.Type != AuthPassthrough {
		errs = append(errs, n+": auth.login needs auth.type passthrough")
	}
	errs = append(errs, validateAuthType(n, p)...)
	errs = append(errs, validatePreset(n, p)...)
	errs = append(errs, validateEnv(n, p.Env)...)
	if p.OpencodeFallback == "auto" {
		errs = append(errs, n+": opencode_fallback cannot itself be \"auto\"")
	}
	return errs
}

// validateNameRefs checks default/fallback names against known providers (opencode ladder names are
// checked by validateRouting, in routing.go).
func validateNameRefs(r *Registry) []string {
	var errs []string
	for _, n := range append([]string{r.Default}, r.Fallback...) {
		if _, ok := r.Providers[n]; n != "" && !ok {
			errs = append(errs, fmt.Sprintf("default/fallback names unknown provider %q", n))
		}
	}
	return errs
}
