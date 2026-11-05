package databricks

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

//go:embed templates/*
var tmplFS embed.FS

const (
	// PlaceholderHost is the host baked into the embedded templates.
	PlaceholderHost = "https://example.cloud.databricks.com"
	WireAIGateway   = providers.WireAIGateway
	AuthHarness     = "harness"
	AuthCLI         = "cli"
	TargetClaude    = "claude"
	TargetCodex     = "codex"
	// GatewayNote is shown for gateway-wire output.
	GatewayNote = "gateway inference is budget-denied on this account (403); serving is the working path"
	authHeader  = "[model_providers.databricks.auth]"
	helperTTL   = "300000"
)

// Options is one render request. P is the expanded registry row: routes, headers, env and model prefix all
// come from it. Self is the claude-code binary the harness token helper runs.
type Options struct {
	Target, Host, Auth, Self string
	Model                    string // optional model override, canonical or wire id
	P                        *providers.Provider
	Set, Unset               []string
}

// Result is the rendered file plus an optional note the caller shows out-of-band (stderr).
type Result struct {
	Text string
	Note string
}

var credSegs = map[string]bool{"TOKEN": true, "KEY": true, "SECRET": true, "PASSWORD": true, "PASSWD": true, "CREDENTIAL": true, "CREDENTIALS": true}

// CredentialShaped reports whether a config key names a secret, by underscore/dash/dot segment.
func CredentialShaped(k string) bool {
	for _, s := range strings.FieldsFunc(strings.ToUpper(k), func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if credSegs[s] {
			return true
		}
	}
	return false
}

// Render produces the config text for o.
func Render(o Options) (Result, error) {
	if err := o.validate(); err != nil {
		return Result{}, err
	}
	var res Result
	var err error
	if o.Target == TargetClaude {
		res, err = renderClaude(o)
	} else {
		res, err = renderCodex(o)
	}
	if err == nil && o.P.Options.Wire == WireAIGateway {
		res.Note = GatewayNote
	}
	return res, err
}

var (
	hostRe   = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:\d+)?$`)
	setKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

func (o Options) validate() error {
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
func (o Options) checkBasics() error {
	if o.Target != TargetClaude && o.Target != TargetCodex {
		return fmt.Errorf("target must be claude or codex (got %q)", o.Target)
	}
	if o.P == nil {
		return errors.New("no provider row to render from")
	}
	if o.Auth != AuthHarness && o.Auth != AuthCLI {
		return fmt.Errorf("--auth must be harness or cli (got %q)", o.Auth)
	}
	if !hostRe.MatchString(o.Host) {
		return fmt.Errorf("host must be https://host with no path (got %q)", o.Host)
	}
	if o.Auth == AuthHarness && (o.P.Name == "" || o.Self == "") {
		return errors.New("--auth harness needs a named provider and the claude-code binary path")
	}
	return nil
}

// checkSetPair validates one --set K=V pair: shape, key charset, and no credential-shaped key.
func checkSetPair(kv string) error {
	k, _, ok := strings.Cut(kv, "=")
	if !ok || k == "" {
		return fmt.Errorf("--set wants K=V (got %q)", kv)
	}
	if !setKeyRE.MatchString(k) {
		return fmt.Errorf("--set %q: keys must match [A-Za-z0-9_.-]+ (a bare TOML/env key)", k)
	}
	if CredentialShaped(k) {
		return fmt.Errorf("--set %s: credential keys are refused; auth comes from the token helper", k)
	}
	return nil
}

// checkModel enforces which models each target can reach without the router.
func (o Options) checkModel() error {
	if o.Model == "" {
		return nil
	}
	isClaude := models.FamilyKind(o.Model) == models.FamilyClaude
	if o.Target == TargetCodex && isClaude {
		return fmt.Errorf("model %q: the Responses route serves GPT models only", o.Model)
	}
	if o.Target != TargetClaude {
		return nil
	}
	if err := providers.ClaudeCodeError(o.P, o.Model); err != nil {
		return err
	}
	wire, ok, why := providers.WireFor(o.P, o.Model)
	if !ok && !isClaude {
		return fmt.Errorf("model %q: %s", o.Model, why)
	}
	if ok && wire != providers.ProtocolAnthropic {
		return fmt.Errorf("model %q: a non-Claude model for Claude Code needs the router, which break-glass avoids", o.Model)
	}
	return nil
}

// tmpl loads an embedded template with its placeholder host swapped for host.
func tmpl(name, host string) (string, error) {
	b, err := tmplFS.ReadFile("templates/" + name)
	return strings.ReplaceAll(string(b), PlaceholderHost, host), err
}

// cliTokenCmd is the template's own Databricks CLI token command.
func cliTokenCmd(host string) string {
	return "databricks auth token --host " + host + " --output json | jq -r '.access_token'"
}

// headerLines renders p.Headers as ANTHROPIC_CUSTOM_HEADERS lines, sorted.
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
			if !CredentialShaped(k) {
				continue
			}
		}
		return fmt.Errorf("header %q looks like a credential; auth comes from the token helper, not a rendered header", k)
	}
	return nil
}

func renderClaude(o Options) (Result, error) {
	raw, err := tmpl("claude.json", o.Host)
	if err != nil {
		return Result{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return Result{}, err
	}
	env, _ := doc["env"].(map[string]any)
	if env == nil {
		return Result{}, errors.New("claude template has no env block")
	}
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_USE_GATEWAY", "ANTHROPIC_AUTH_TOKEN"} {
		delete(env, k)
	}
	if err := claudeProviderEnv(env, o); err != nil {
		return Result{}, err
	}
	doc["apiKeyHelper"] = cliTokenCmd(o.Host)
	if o.Auth == AuthHarness {
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
	return Result{Text: string(b) + "\n"}, err
}

// claudeProviderEnv layers the provider's route, headers, models and env onto the template env block.
func claudeProviderEnv(env map[string]any, o Options) error {
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
	if p.Options.Wire == WireAIGateway {
		env["CLAUDE_CODE_USE_GATEWAY"] = "1"
	}
	if !p.Serves1M() {
		env["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
	}
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		env["ANTHROPIC_DEFAULT_"+strings.ToUpper(alias)+"_MODEL"] = p.WireModel(aliasID(alias))
	}
	if o.Model != "" {
		env["ANTHROPIC_MODEL"] = p.WireModel(bareModel(o.Model))
	}
	for k, v := range p.Env {
		env[k] = v
	}
	env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] = "1"
	env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = helperTTL
	return nil
}

// tomlNumberRE is a TOML decimal integer or float; Go's ParseFloat also takes Infinity and hex floats, TOML does not.
var tomlNumberRE = regexp.MustCompile(`^[+-]?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// tomlValue writes v raw when it is a bool or number, else as a quoted string.
func tomlValue(v string) string {
	if tomlNumberRE.MatchString(v) || v == "true" || v == "false" {
		return v
	}
	return strconv.Quote(v)
}

// replaceLine rewrites the first `key = ...` line of head to key = val.
func replaceLine(head, key, val string) string {
	lines := strings.Split(head, "\n")
	for i, l := range lines {
		if strings.Contains(l, "=") && keyOf(l) == key {
			lines[i] = key + " = " + strconv.Quote(val)
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

// bareModel strips either wire's id prefix, so a serving id given for a gateway row (or back) is re-prefixed.
func bareModel(id string) string {
	for _, prefix := range []string{"databricks-", "system.ai."} {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			return rest
		}
	}
	return id
}

func renderCodex(o Options) (Result, error) {
	raw, err := tmpl("codex.toml", o.Host)
	if err != nil {
		return Result{}, err
	}
	p := o.P
	base, ok := p.Route(providers.ProtocolResponses)
	if !ok {
		return Result{}, fmt.Errorf("provider %q has no Responses route", p.Name)
	}
	head, _, _ := strings.Cut(raw, authHeader)
	head = replaceLine(head, "base_url", base)
	model := "gpt-5.5"
	if o.Model != "" {
		model = o.Model
	}
	head = replaceLine(head, "model", p.WireModel(bareModel(model)))
	if p.Options.Wire != WireAIGateway {
		head = replaceLine(head, "name", "Databricks serving")
	}
	auth := fmt.Sprintf("command = \"sh\"\nargs = [\"-c\", %s]\n", strconv.Quote(cliTokenCmd(o.Host)))
	if o.Auth == AuthHarness {
		auth = fmt.Sprintf("command = %s\nargs = [\"resolve\", \"--profile\", %s]\n", strconv.Quote(o.Self), strconv.Quote(p.Name))
	}
	auth += "timeout_ms = 5000\nrefresh_interval_ms = 1800000\n"
	text := editTopLevel(head, o) + authHeader + "\n" + auth
	if p.Options.Wire == WireAIGateway {
		text = "# " + GatewayNote + "\n" + text
	}
	return Result{Text: text}, nil
}

// keyOf returns the trimmed key of a `k = v` line.
func keyOf(l string) string {
	k, _, _ := strings.Cut(l, "=")
	return strings.TrimSpace(k)
}

// editTopLevel applies --set/--unset to the keys above the first [table] of a TOML head.
func editTopLevel(head string, o Options) string {
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
