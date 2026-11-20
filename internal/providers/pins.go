package providers

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/structdiff"
	"github.com/edotau/claude-code/internal/vault"
)

// EnvSessionProfile is the env var a launcher stamps into a session's child env so its statusline and hooks see it.
const EnvSessionProfile = "CLAUDE_PROFILE"

// agentRecords reads every stored agent record in one vault transaction.
func agentRecords() (recs map[string]vault.AgentRecord, err error) {
	err = vault.View(func(tx *vault.Tx) (e error) { recs, e = tx.AgentRecords(); return e })
	return recs, err
}

// AgentRecords is every stored agent record from one vault read (for packages that cannot import internal/agent).
func AgentRecords() map[string]vault.AgentRecord {
	recs, err := agentRecords()
	warnVault("agent records", err)
	return recs
}

// warnVault tells the user a vault read failed, so an empty result is not mistaken for "unset".
func warnVault(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: vault %s unreadable, using defaults: %v\n", what, err)
	}
}

// AgentRecord is agent's vault record (zero value when unset or unreadable).
func AgentRecord(agent string) vault.AgentRecord { return AgentRecords()[agent] }

// CopilotModel is the copilot launch, configure page and doctor resolver: the record's model, else github's opus alias.
func CopilotModel(reg *Registry) (model, source string) {
	if m := AgentRecord("copilot").Model; m != "" {
		return m, "pin"
	}
	if reg == nil {
		return "", "github-copilot default"
	}
	if names := reg.ProfilesOf("github"); len(names) > 0 {
		return reg.Providers[names[0]].Models[models.Opus], "github-copilot default"
	}
	return "", "github-copilot default"
}

// recordProfile is the registry row r's route names; a pool pick ("auto") is its provider group.
func recordProfile(r vault.AgentRecord) string {
	rt := r.EffectiveRoute()
	if rt.Profile == "auto" {
		return strings.TrimSpace(rt.Provider)
	}
	return strings.TrimSpace(rt.Profile)
}

// CopilotLogin is the copilot record's github profile Login and name; ("", "") = gh's account; ignores github-auth.
func CopilotLogin(reg *Registry) (login, profile string) {
	pin := recordProfile(AgentRecord("copilot"))
	if pin == "" {
		return "", ""
	}
	if reg != nil {
		if p := reg.Providers[pin]; p != nil && p.Provider == "github" {
			return p.Login, pin
		}
	}
	return "", ""
}

// GithubAccountName is the route-account name (github·<name>) for a gh login: its profile, else the login itself.
func GithubAccountName(reg *Registry, login string) string {
	if reg != nil && login != "" {
		for _, n := range reg.ProfilesOf("github") {
			if reg.Providers[n].Login == login {
				return n
			}
		}
	}
	return login
}

// GithubAccountLogin is the gh login behind a route-account name: the profile's Login, else name as a raw login.
func GithubAccountLogin(reg *Registry, name string) string {
	if reg != nil {
		if p := reg.Providers[name]; p != nil && p.Provider == "github" {
			return p.Login
		}
	}
	return name
}

// AgentModelEnv is the agent's own shell model knob (GEMINI_MODEL), "" for claude and unknown agents.
func AgentModelEnv(agent string) string {
	if !slices.Contains(agentid.External(), agent) {
		return ""
	}
	return strings.ToUpper(agent) + "_MODEL"
}

// EnvModel is the agent's shell model env value in ctx's run snapshot (the process env without one), with its var name.
func EnvModel(ctx context.Context, agent string) (model, envVar string) {
	envVar = AgentModelEnv(agent)
	if envVar == "" {
		return "", ""
	}
	return strings.TrimSpace(proc.Value(ctx, envVar)), envVar
}

// effectiveModel is the launch model: the shell env over the record's model; source is "env VAR", "pin" or "default".
func effectiveModel(agent string) (model, source string) {
	if m, v := EnvModel(context.Background(), agent); m != "" {
		return m, "env " + v
	}
	return pinned(AgentRecord(agent).Model)
}

// CodexModel resolves the model a codex launch uses (shell env, then the record); "" leaves codex's own default.
func CodexModel() (model, source string) { return effectiveModel("codex") }

// CodexEffort resolves the codex record's effort: it if set, else "" — codex's own default effort.
func CodexEffort() (effort, source string) {
	return pinned(AgentRecord("codex").Effort)
}

// GeminiModel resolves the model a gemini launch uses (shell env, then the record); "" leaves agy's own default.
func GeminiModel() (model, source string) { return effectiveModel("gemini") }

// pinned is a record field and its source: "pin" when set, else "" and "default".
func pinned(v string) (value, source string) {
	if v != "" {
		return v, "pin"
	}
	return "", "default"
}

// ReadEnvFile parses KEY=VALUE lines; a missing file is empty, any other read error is noted on stderr (path only).
func ReadEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: env file %s unreadable: %v\n", path, err)
		}
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if k, v, ok := parseEnvLine(sc.Text()); ok {
			out[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: env file %s read stopped early: %v\n", path, err)
	}
	return out
}

// parseEnvLine parses one KEY=VALUE line; ok is false for blanks, comments and lines without "=".
func parseEnvLine(line string) (k, v string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, ok = strings.Cut(strings.TrimPrefix(line, "export "), "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), vault.UnquoteDotenv(v), true
}

// AuditRecords audits claude's record changes after their transaction succeeds: a profile switch, a model pin or unpin.
func AuditRecords(before, after map[string]vault.AgentRecord) {
	b, n := recordFields(before["claude"]), recordFields(after["claude"])
	for _, f := range slices.Sorted(maps.Keys(mergeKeys(b, n))) {
		switch {
		case b[f] == n[f]:
		case f == "profile":
			audit.Log(audit.ProviderSwitch, n[f], pinCaller())
		case f == "model" || strings.HasPrefix(f, "aliases."):
			audit.Log(audit.ModelPin, "claude."+f, cmp.Or(n[f], "unpinned"))
		}
	}
}

// AuditSwitch records a session-profile switch.
func AuditSwitch(profile string) { audit.Log(audit.ProviderSwitch, profile, pinCaller()) }

// AuditModelPin records a claude model pin for alias ("" = every alias); an empty spec is an unpin.
func AuditModelPin(alias, spec string) {
	key := "claude.model"
	if alias != "" {
		key = "claude.aliases." + alias
	}
	audit.Log(audit.ModelPin, key, cmp.Or(spec, "unpinned"))
}

// recordDiff lists "agent.path: old → new" for every field that differs between before and after, secrets redacted.
func recordDiff(reg *Registry, before, after map[string]vault.AgentRecord) []string {
	var out []string
	for _, c := range structdiff.Diff(routeForm(reg, before), routeForm(reg, after)) {
		out = append(out, structdiff.Line(c, secretPath))
	}
	return out
}

// routeForm spells every record in the route form, so a legacy record and its migrated twin diff as equal.
func routeForm(reg *Registry, recs map[string]vault.AgentRecord) map[string]vault.AgentRecord {
	out := make(map[string]vault.AgentRecord, len(recs))
	for a, r := range recs {
		r.Route, r.FallbackRoute = CompleteRoute(reg, r.EffectiveRoute()), CompleteRoute(reg, r.EffectiveFallbackRoute())
		r.Profile, r.Account, r.Fallback = "", "", ""
		out[a] = r
	}
	return out
}

// CompleteRoute fills a bare-profile route's provider from the registry: "databricks" is the pool, else the row's provider.
func CompleteRoute(reg *Registry, rt route.Route) route.Route {
	switch {
	case rt.Provider != "" || rt.Profile == "" || reg == nil:
	case rt.Profile == "databricks":
		return route.Route{Provider: "databricks", Profile: "auto"}
	case reg.Providers[rt.Profile] != nil:
		rt.Provider = reg.Providers[rt.Profile].Provider
	}
	return rt
}

// secretPath marks launch env (KEY=VALUE pairs), the one record field that can carry a value.
func secretPath(path string) bool {
	_, rest, _ := strings.Cut(path, ".")
	return rest == "launch.env"
}

func mergeKeys[V any](a, b map[string]V) map[string]struct{} {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// recordFields flattens r to field -> value, aliases as aliases.<alias>.
func recordFields(r vault.AgentRecord) map[string]string {
	f := map[string]string{"profile": recordProfile(r), "model": r.Model, "effort": r.Effort, "context": r.Context,
		"account": r.EffectiveRoute().Account}
	for alias, v := range r.Aliases {
		f["aliases."+alias] = v
	}
	return f
}

// RenderPins renders the vault pins table as the plain-text snapshot history records.
func RenderPins() ([]byte, error) {
	var pins map[string]string
	err := vault.View(func(tx *vault.Tx) error { pins = tx.All(vault.Pins); return nil })
	if err != nil {
		return nil, err
	}
	if len(pins) == 0 {
		return nil, fs.ErrNotExist
	}
	keys := slices.Sorted(maps.Keys(pins))
	var b strings.Builder
	b.WriteString("# Managed by `claude-code switch` / `claude-code models --pin`.\n")
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(pins[k])
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// OAuthTokenEnv is the env var Claude Code reads a long-lived Max-plan token from (`claude setup-token` mints one).
const OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// HeadlessOAuthToken reads the setup-token from the process env, then the vault.
func HeadlessOAuthToken() string {
	if tok := strings.TrimSpace(os.Getenv(OAuthTokenEnv)); tok != "" {
		return tok
	}
	return StoredSecret(OAuthTokenEnv)
}

// StoredSecret reads a vault secret; a miss or read error yields an empty string, the error noted on stderr.
func StoredSecret(name string) string {
	v, _, err := vault.ReadSecret(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: vault secret %s unreadable: %v\n", name, err)
		return ""
	}
	return v
}

// HasSecret reports env or vault presence without resolving a master key; a vault read error reads as absent.
func HasSecret(name string) string {
	source, _ := vault.HasSecretErr(name)
	return source
}

// pinCaller names who wrote a pin — the verb and the parent process — so an unexpected switch can be traced.
func pinCaller() string {
	verb := ""
	if len(os.Args) > 1 {
		verb = os.Args[1]
	}
	return "via " + cmp.Or(verb, "?") + " (parent " + cmp.Or(proc.Comm(os.Getppid()), "?") + ")"
}

// SpellPin is pin spelled the way p serves it (a family as its concrete id), or "" when p cannot serve it.
func SpellPin(p *Provider, pin string) string {
	if IsFamily(pin) {
		return ConcreteModel("", p, pin)
	}
	if p == nil {
		return pin
	}
	if p.Preset != PresetDatabricks && strings.HasPrefix(pin, models.DatabricksPrefix) {
		return ""
	}
	return p.WireModel(models.Strip1M(pin))
}

// copilotPlans maps GitHub's copilot_plan values to the plan names GitHub sells.
var copilotPlans = map[string]string{
	"free": "Free", "individual": "Pro", "individual_pro": "Pro+", "business": "Business", "enterprise": "Enterprise",
}

// CopilotTokenEnv is copilot's highest-precedence token var; the harness sets it at launch from `gh auth token`.
const CopilotTokenEnv = "COPILOT_GITHUB_TOKEN"

// CopilotPlanName is the sold name of a copilot_plan value ("individual_pro" → "Pro+"); the raw value when unknown.
func CopilotPlanName(raw string) string {
	if name, ok := copilotPlans[raw]; ok {
		return name
	}
	return raw
}

// QualifyPinSpec is spec as "profile:model" unless providerFlag is the profile Select resolves (record, else default).
func QualifyPinSpec(reg *Registry, providerFlag, spec string) string {
	if providerFlag == "" || spec == "" {
		return spec
	}
	if ms, err := SplitModelSpec(reg, spec); err == nil && ms.Row != "" {
		return spec
	}
	if providerFlag == cmp.Or(recordProfile(AgentRecord("claude")), reg.Default) {
		return spec
	}
	return providerFlag + ":" + spec
}

// ParseModelPins maps `alias=model` pins (plus unpin) to alias → spec: alias "" is every alias, spec "" unpins.
func ParseModelPins(reg *Registry, profile string, unpin bool, pins []string) (map[string]string, error) {
	set := map[string]string{}
	if unpin {
		set[""] = ""
		for _, s := range models.AliasNames {
			set[s] = ""
		}
	}
	for _, p := range pins {
		alias, spec, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("--pin wants alias=model, got %q", p)
		}
		if alias == "all" {
			alias = ""
		} else if !slices.Contains(models.AliasNames, alias) {
			return nil, fmt.Errorf("unknown alias %q (opus|sonnet|haiku|fable|all)", alias)
		}
		set[alias] = QualifyPinSpec(reg, profile, spec)
	}
	return set, nil
}

// WithClaudeModels is r with the model pins in set applied: alias "" is every alias, "" spec clears.
func WithClaudeModels(r vault.AgentRecord, set map[string]string) vault.AgentRecord {
	r.Aliases = maps.Clone(r.Aliases)
	if r.Aliases == nil {
		r.Aliases = map[string]string{}
	}
	for alias, spec := range set {
		switch {
		case alias == "":
			r.Model = spec
		case spec == "":
			delete(r.Aliases, alias)
		default:
			r.Aliases[alias] = spec
		}
	}
	return r
}

// ApplyClaudeModels writes set onto claude's record in one vault update, then audits each pin.
func ApplyClaudeModels(set map[string]string) error {
	err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) { m["claude"] = WithClaudeModels(m["claude"], set) })
	if err != nil {
		return err
	}
	for _, alias := range slices.Sorted(maps.Keys(set)) {
		AuditModelPin(alias, set[alias])
	}
	return nil
}

// SessionProfileContext is the profile of the session a run belongs to ("" outside a launched session).
func SessionProfileContext(ctx context.Context) string {
	return strings.TrimSpace(proc.Value(ctx, EnvSessionProfile))
}

// SessionLeadContext is the session's profile, else (withRecord) claude's record profile; a non-nil reg drops unknowns.
func SessionLeadContext(ctx context.Context, reg *Registry, withRecord bool) string {
	name := SessionProfileContext(ctx)
	if name == "" && withRecord {
		name = recordProfile(AgentRecord("claude"))
	}
	if reg != nil && reg.Providers[name] == nil {
		return ""
	}
	return name
}

// SessionName is the /p/<name> proxy base of a routed launch, else the session profile (env, pin), else the default.
func SessionName() (name string, routed bool) {
	if u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL")); err == nil {
		if rest, ok := strings.CutPrefix(u.Path, "/p/"); ok && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
			name, _, _ = strings.Cut(rest, "/")
			return name, true
		}
	}
	if p := SessionLeadContext(context.Background(), nil, true); p != "" {
		return p, false
	}
	if reg, _ := LoadShared(); reg != nil {
		return reg.Default, false
	}
	return "", false
}

// SessionProvider is the provider SessionName resolves, nil when the registry does not know it.
func SessionProvider() *Provider {
	name, _ := SessionName()
	if reg, _ := Load(); reg != nil {
		return reg.Providers[name]
	}
	return nil
}
