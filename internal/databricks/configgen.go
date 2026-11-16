package databricks

import (
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

//go:embed templates/*
var tmplFS embed.FS

const (
	placeholderHost = "https://example.cloud.databricks.com"
	authHarness     = "harness"
	authCLI         = "cli"
	targetClaude    = "claude"
	targetCodex     = "codex"
	authHeader      = "[model_providers.databricks.auth]"
	helperTTL       = "300000"
)

// renderOptions is one render request; P is the expanded registry row and Self the token helper's binary.
type renderOptions struct {
	Target, Host, Auth, Self string
	Model                    string // optional model override, canonical or wire id
	P                        *providers.Provider
	Set, Unset               []string
}

type renderResult struct {
	Text string
}

func renderConfig(o renderOptions) (renderResult, error) {
	if err := o.validate(); err != nil {
		return renderResult{}, err
	}
	if o.Target == targetClaude {
		return renderClaude(o)
	}
	return renderCodex(o)
}

var (
	hostRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:\d+)?$`) })
	setKeyRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.-]+$`) })
)

func (o renderOptions) validate() error {
	if err := o.checkBasics(); err != nil {
		return err
	}
	for _, kv := range o.Set {
		if err := checkSetPair(kv); err != nil {
			return err
		}
	}
	return o.checkModel()
}

// checkBasics validates the target, provider row, auth mode, host and harness prerequisites, in that order.
func (o renderOptions) checkBasics() error {
	if o.Target != targetClaude && o.Target != targetCodex {
		return fmt.Errorf("target must be claude or codex (got %q)", o.Target)
	}
	if o.P == nil {
		return errors.New("no provider row to render from")
	}
	return o.checkAuthHost()
}

// checkAuthHost validates the auth mode, the host and the harness token helper's prerequisites.
func (o renderOptions) checkAuthHost() error {
	if o.Auth != authHarness && o.Auth != authCLI {
		return fmt.Errorf("--auth must be harness or cli (got %q)", o.Auth)
	}
	if !hostRe().MatchString(o.Host) {
		return fmt.Errorf("host must be https://host with no path (got %q)", o.Host)
	}
	if o.Auth == authHarness && (o.P.Name == "" || o.Self == "") {
		return errors.New("--auth harness needs a named provider and the claude-code binary path")
	}
	return nil
}

// checkSetPair validates one --set K=V pair: shape, key charset, and no credential-shaped key or value.
func checkSetPair(kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok || k == "" {
		return fmt.Errorf("--set wants K=V (got %q)", kv)
	}
	if !setKeyRE().MatchString(k) {
		return fmt.Errorf("--set %q: keys must match [A-Za-z0-9_.-]+ (a bare TOML/env key)", k)
	}
	if credLeaf(k) && !strings.HasSuffix(k, "_TTL_MS") {
		return fmt.Errorf("--set %s: credential keys are refused; auth comes from the token helper", k)
	}
	if credValue(v) {
		return fmt.Errorf("--set %s: the value looks like a credential; auth comes from the token helper", k)
	}
	return nil
}

func (o renderOptions) checkModel() error {
	if o.Model == "" {
		return nil
	}
	isClaude := models.FamilyKind(o.Model) == models.FamilyClaude
	if o.Target == targetCodex && isClaude {
		return fmt.Errorf("model %q: the Responses route serves GPT models only", o.Model)
	}
	if o.Target == targetCodex {
		return o.checkCodexWire()
	}
	if err := providers.ClaudeCodeError(o.P, o.Model); err != nil {
		return err
	}
	return o.checkClaudeWire(isClaude)
}

// checkCodexWire refuses a catalog model the Responses route does not serve (gpt-oss); an unlisted id is the user's.
func (o renderOptions) checkCodexWire() error {
	if _, _, known := models.Resolve(o.Model); known && !providers.ResponsesServable(o.P, o.P.WireModel(o.Model)) {
		return fmt.Errorf("model %q: the Responses route does not serve it; use a GPT model", o.Model)
	}
	return nil
}

// checkClaudeWire refuses a model Claude Code could reach only through the router.
func (o renderOptions) checkClaudeWire(isClaude bool) error {
	wire, ok, why := providers.WireFor(o.P, o.Model)
	if !ok && !isClaude {
		return fmt.Errorf("model %q: %s", o.Model, why)
	}
	if ok && wire != providers.ProtocolAnthropic {
		return fmt.Errorf("model %q: a non-Claude model for Claude Code needs the router, which break-glass avoids", o.Model)
	}
	return nil
}

// tmpl loads an embedded template; its placeholder host never survives, every URL is re-set from the provider row.
func tmpl(name string) (string, error) {
	b, err := tmplFS.ReadFile("templates/" + name)
	return string(b), err
}

// cliTokenCmd is the Databricks CLI token command a rendered config runs for host.
func cliTokenCmd(host string) string {
	return "databricks auth token --host " + host + " --output json | jq -r '.access_token'"
}

func headerLines(h map[string]string) string {
	var ls []string
	for k, v := range h {
		ls = append(ls, k+": "+v)
	}
	sort.Strings(ls)
	return strings.Join(ls, "\n")
}

// checkHeaders refuses a header whose name marks a credential: the rendered file must hold no token.
func checkHeaders(h map[string]string) error {
	for k := range h {
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "cookie":
		default:
			if !credLeaf(k) {
				continue
			}
		}
		return fmt.Errorf("header %q looks like a credential; auth comes from the token helper, not a rendered header", k)
	}
	for k, v := range h {
		if credValue(v) {
			return fmt.Errorf("header %q value looks like a credential; auth comes from the token helper", k)
		}
	}
	return nil
}

// checkProviderEnv refuses a provider env entry whose key or value is credential-shaped, as --set does.
func checkProviderEnv(e map[string]string) error {
	for k, v := range e {
		if (credLeaf(k) && !strings.HasSuffix(k, "_TTL_MS")) || credValue(v) {
			return fmt.Errorf("provider env %q looks like a credential; auth comes from the token helper", k)
		}
	}
	return nil
}

func renderClaude(o renderOptions) (renderResult, error) {
	raw, err := tmpl("claude.json")
	if err != nil {
		return renderResult{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return renderResult{}, err
	}
	env, _ := doc["env"].(map[string]any)
	if env == nil {
		return renderResult{}, errors.New("claude template has no env block")
	}
	delete(env, "ANTHROPIC_MODEL") // the template's model is dropped; claudeProviderEnv sets one only for --model
	if err := claudeProviderEnv(env, o); err != nil {
		return renderResult{}, err
	}
	doc["apiKeyHelper"] = cliTokenCmd(o.Host)
	if o.Auth == authHarness {
		doc["apiKeyHelper"] = textutil.QuoteSH(o.Self) + " resolve --profile " + textutil.QuoteSH(o.P.Name)
	}
	for _, kv := range o.Set {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for _, k := range o.Unset {
		delete(env, k)
	}
	delete(env, "ANTHROPIC_AUTH_TOKEN")
	b, err := json.MarshalIndent(doc, "", "  ")
	return renderResult{Text: string(b) + "\n"}, err
}

// claudeProviderEnv layers the provider's route, headers, models and env onto the template env block.
func claudeProviderEnv(env map[string]any, o renderOptions) error {
	p := o.P
	base, ok := p.Route(providers.ProtocolAnthropic)
	if !ok {
		return fmt.Errorf("provider %q has no Anthropic route", p.Name)
	}
	env["ANTHROPIC_BASE_URL"] = base
	if err := checkHeaders(p.Headers); err != nil {
		return err
	}
	if h := headerLines(p.Headers); h != "" {
		env["ANTHROPIC_CUSTOM_HEADERS"] = h
	}
	if !p.Serves1M() {
		env["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
	}
	for _, alias := range models.AliasNames {
		env["ANTHROPIC_DEFAULT_"+strings.ToUpper(alias)+"_MODEL"] = p.WireModel(cmp.Or(p.Models[alias], aliasID(alias)))
	}
	env["CLAUDE_CODE_AUTO_MODE_MODEL"] = p.WireModel(cmp.Or(p.Models[models.Opus], aliasID(models.Opus))) // classifier default is unserved here
	if o.Model != "" {
		env["ANTHROPIC_MODEL"] = p.WireModel(o.Model)
	}
	if err := checkProviderEnv(p.Env); err != nil {
		return err
	}
	for k, v := range p.Env {
		env[k] = v
	}
	env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] = "1"
	env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = helperTTL
	return nil
}

// tomlNumberRE is a TOML decimal integer or float; Go's ParseFloat also takes Infinity and hex floats, TOML does not.
var tomlNumberRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[+-]?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
})

func tomlValue(v string) string {
	if tomlNumberRE().MatchString(v) || v == "true" || v == "false" {
		return v
	}
	return agentconfig.TOMLQuoteRaw(v)
}

func replaceLine(head, key, val string) string {
	lines := strings.Split(head, "\n")
	for i, l := range lines {
		if strings.Contains(l, "=") && keyOf(l) == key {
			lines[i] = key + " = " + agentconfig.TOMLQuoteRaw(val)
			break
		}
	}
	return strings.Join(lines, "\n")
}

// aliasID is the canonical id an alias (opus, sonnet, haiku) currently backs; the alias itself if unknown.
func aliasID(alias string) string {
	if id, ok := models.Alias(alias); ok {
		return id
	}
	return alias
}

func renderCodex(o renderOptions) (renderResult, error) {
	raw, err := tmpl("codex.toml")
	if err != nil {
		return renderResult{}, err
	}
	p := o.P
	base, ok := p.Route(providers.ProtocolResponses)
	if !ok {
		return renderResult{}, fmt.Errorf("provider %q has no Responses route", p.Name)
	}
	head, _, _ := strings.Cut(raw, authHeader)
	head = replaceLine(head, "base_url", base)
	head = replaceLine(head, "model", p.WireModel(cmp.Or(o.Model, "gpt-6.1-sol")))
	auth := fmt.Sprintf("command = \"sh\"\nargs = [\"-c\", %s]\n", agentconfig.TOMLQuoteRaw(cliTokenCmd(o.Host)))
	if o.Auth == authHarness {
		auth = fmt.Sprintf("command = %s\nargs = [\"resolve\", \"--profile\", %s]\n", agentconfig.TOMLQuoteRaw(o.Self), agentconfig.TOMLQuoteRaw(p.Name))
	}
	auth += "timeout_ms = 5000\nrefresh_interval_ms = 1800000\n"
	text := editTopLevel(head, o) + authHeader + "\n" + auth
	return renderResult{Text: text}, nil
}

func keyOf(l string) string {
	k, _, _ := strings.Cut(l, "=")
	return strings.TrimSpace(k)
}

// editTopLevel applies --set/--unset to the keys above the first [table] of a TOML head.
func editTopLevel(head string, o renderOptions) string {
	top, rest := head, ""
	if i := strings.Index(head, "\n["); i >= 0 {
		top, rest = head[:i+1], head[i+1:]
	}
	var lines []string
	for l := range strings.SplitSeq(strings.TrimRight(top, "\n"), "\n") {
		if !strings.Contains(l, "=") || !slices.Contains(o.Unset, keyOf(l)) {
			lines = append(lines, l)
		}
	}
	for _, kv := range o.Set {
		k, v, _ := strings.Cut(kv, "=")
		lines = setTopLevel(lines, k, v)
	}
	out := strings.Join(lines, "\n") + "\n"
	if rest != "" {
		out += "\n" + rest
	}
	return out
}

// setTopLevel rewrites every `k = ...` line to the TOML value v, or appends the pair when none exists.
func setTopLevel(lines []string, k, v string) []string {
	replaced := false
	for j, l := range lines {
		if strings.Contains(l, "=") && keyOf(l) == k {
			lines[j], replaced = k+" = "+tomlValue(v), true
		}
	}
	if !replaced {
		lines = append(lines, k+" = "+tomlValue(v))
	}
	return lines
}
