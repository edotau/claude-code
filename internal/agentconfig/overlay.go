// overlay.go — COWORKER_OVERLAY: one dispatch's keyed config (routing keys + a set/unset/keep env layer).
package agentconfig

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// OverlayVar names the overlay (a file path, inline JSON or -) every dispatch this process starts reads.
const OverlayVar = "COWORKER_OVERLAY"

// overlaySchema versions the overlay document; one without "schema" reads as this version.
const overlaySchema = 1

const overlayMaxBytes = 64 << 10

// Overlay is one dispatch's config document: where it runs plus its env layer; Files is reserved and refused.
type Overlay struct {
	Schema   int               `json:"schema,omitempty"`
	Provider string            `json:"provider,omitempty"`
	Profile  string            `json:"profile,omitempty"`
	Model    string            `json:"model,omitempty"`
	Effort   string            `json:"effort,omitempty"`
	Fallback string            `json:"fallback,omitempty"`
	Host     string            `json:"host,omitempty"`
	Remote   string            `json:"remote,omitempty"`
	Set      map[string]string `json:"set,omitempty"`
	Unset    []string          `json:"unset,omitempty"`
	Keep     []string          `json:"keep,omitempty"`
	Files    json.RawMessage   `json:"files,omitempty"`
}

// routeKey is one routing key: its JSON name, the typed flag it folds under ("" = none), its field and its rule.
type routeKey struct {
	name, flag string
	get        func(Overlay) string
	ok         func(v string) bool // nil = any text
	msg        string              // the refusal after "<name>: "; never quotes the value
}

// routeKeys defines every routing key once: validation, Fold, Routed and RoutingKeys all read this table.
var routeKeys = []routeKey{
	{"provider", "", func(o Overlay) string { return o.Provider }, in(providers.ProviderKeys()),
		"unknown (" + strings.Join(providers.ProviderKeys(), "|") + ")"},
	{"profile", "", func(o Overlay) string { return o.Profile }, nil, ""},
	{"model", "model", func(o Overlay) string { return o.Model }, nil, ""},
	{"effort", "effort", func(o Overlay) string { return o.Effort }, in(agentid.EffortNames()),
		"want " + strings.Join(agentid.EffortNames(), ", ")},
	{"fallback", "fallback", func(o Overlay) string { return o.Fallback }, in(router.FallbackModes),
		"want " + strings.Join(router.FallbackModes, ", ")},
	{"host", "host", func(o Overlay) string { return o.Host }, sshHost, "not a valid ssh host"},
	{"remote", "", func(o Overlay) string { return o.Remote }, sshHost, "not a valid ssh host"},
}

func in(names []string) func(string) bool {
	return func(v string) bool { return slices.Contains(names, v) }
}

func sshHost(v string) bool { return textutil.ValidHost(v) == nil }

// RoutingKeys lists the routing keys in document order.
func RoutingKeys() []string {
	var out []string
	for _, k := range routeKeys {
		out = append(out, k.name)
	}
	return out
}

// Fold sets each routing key on its flag in fs unless that flag was typed or fs lacks it: flag > overlay > default.
func (o Overlay) Fold(fs *flag.FlagSet) {
	typed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { typed[f.Name] = true })
	for _, k := range routeKeys {
		if f := fs.Lookup(k.flag); f != nil && !typed[k.flag] && k.get(o) != "" {
			_ = f.Value.Set(k.get(o)) // not fs.Set: the flag must stay untyped for a later fold or record
		}
	}
}

// Resolved is an overlay with every vault ref swapped for its value; values must never be logged.
type Resolved struct {
	Set         map[string]string
	Unset, Keep []string
}

var overlayFormat = schemas.Register(schemas.Format{
	Name: "coworker-overlay", Owner: "internal/agentconfig", Kind: schemas.Wire, Version: overlaySchema,
	New: func() any { return new(Overlay) }, Migrate: schemas.SameShape,
})

// overlaySrc is a document's error labels and the rules its set entries pass (--env: flagSetRules).
type overlaySrc struct {
	set, keep, unset string
	setRules         []rule
}

var (
	fileSrc = overlaySrc{"set", "keep", "unset", setRules}
	flagSrc = overlaySrc{"--env", "--keep-env", "unset", flagSetRules}
)

// DispatchOverlay is a dispatch's overlay value: flag, else $COWORKER_OVERLAY, else the vault env's (top-level only).
func DispatchOverlay(flag string) string {
	if v := cmp.Or(flag, os.Getenv(OverlayVar)); v != "" || os.Getenv(router.EnvCoworker) != "" {
		return v // a nested dispatch keeps only its inherited confinement
	}
	return strings.TrimSpace(providers.CustomEnv()[OverlayVar])
}

// LoadOverlay reads value, else $COWORKER_OVERLAY: a file path, inline JSON, or - for stdin; "" is the zero overlay.
func LoadOverlay(value string, stdin io.Reader) (Overlay, error) {
	value = cmp.Or(value, os.Getenv(OverlayVar))
	if value == "" {
		return Overlay{}, nil
	}
	where, raw, err := readOverlaySource(value, stdin)
	if err == nil && len(raw) > overlayMaxBytes { // every form: file, stdin, inline and env
		err = fmt.Errorf("over %d bytes", overlayMaxBytes)
	}
	var o Overlay
	if err == nil {
		o, err = parseOverlay(raw)
	}
	if err != nil {
		return Overlay{}, fmt.Errorf("%s: %w", where, err)
	}
	return o, nil
}

// readCapped reads r one byte past overlayMaxBytes: LoadOverlay's cap sees an oversized document, unread.
func readCapped(r io.Reader) ([]byte, error) { return io.ReadAll(io.LimitReader(r, overlayMaxBytes+1)) }

// readOverlaySource returns value's error label and bytes: stdin for -, inline JSON, else the file it names.
func readOverlaySource(value string, stdin io.Reader) (string, []byte, error) {
	where, raw := "overlay", []byte(strings.TrimSpace(value))
	switch {
	case value == "-":
		raw, err := readCapped(stdin)
		if err == nil && len(bytes.TrimSpace(raw)) == 0 {
			err = errEmptyStdin
		}
		return where, raw, err
	case bytes.HasPrefix(raw, []byte("{")):
		return where, raw, nil
	}
	raw, err := readOverlayFile(value)
	if err != errNoOverlay && err != errNoSuchFile { // a mistyped document or path is never echoed
		where += " " + value
	}
	return where, raw, err
}

var (
	errNoOverlay  = errors.New("not a file, inline JSON ({...}) or -")
	errNoSuchFile = errors.New("no such file")
	errEmptyStdin = errors.New("stdin is empty")
)

func readOverlayFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && (strings.Contains(path, "/") || strings.HasSuffix(path, ".json")):
		return nil, errNoSuchFile
	case err != nil:
		return nil, errNoOverlay
	case !info.Mode().IsRegular():
		return nil, errNotRegular // before Open: opening a FIFO would block
	}
	f, err := os.Open(path)
	if err != nil {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			return nil, pe.Err // LoadOverlay names the path once
		}
		return nil, err
	}
	defer f.Close()
	if err := checkOverlayInfo(f.Stat()); err != nil { // the opened file's own mode: no stat-to-read swap
		return nil, err
	}
	return readCapped(f)
}

var errNotRegular = errors.New("not a regular file")

// infoRules are the opened file's refusals, tested in order.
var infoRules = []struct {
	bad func(os.FileInfo) bool
	err error
}{
	{func(i os.FileInfo) bool { return !i.Mode().IsRegular() }, errNotRegular},
	{func(i os.FileInfo) bool { return runtime.GOOS != "windows" && i.Mode().Perm()&0o022 != 0 },
		errors.New("group- or world-writable: chmod go-w it")},
}

func checkOverlayInfo(info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	for _, r := range infoRules {
		if r.bad(info) {
			return r.err
		}
	}
	if uid, ok := paths.OwnerUID(info); ok && uid != 0 && uid != os.Getuid() {
		return errors.New("owned by another user")
	}
	return nil
}

// parseOverlay decodes one document strictly and checks it; errors name keys, never values.
func parseOverlay(raw []byte) (Overlay, error) {
	var o Overlay
	if err := checkOverlaySchema(raw); err != nil {
		return Overlay{}, err
	}
	if _, err := schemas.Decode(overlayFormat, raw, &o); err != nil {
		return Overlay{}, overlayJSONErr(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(new(Overlay)); err != nil {
		return Overlay{}, overlayJSONErr(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Overlay{}, errors.New("trailing data after the JSON object")
	}
	return o, o.validate(fileSrc)
}

// checkOverlaySchema refuses a negative or newer "schema" before schemas.Decode, whose migration loop would walk it.
func checkOverlaySchema(raw []byte) error {
	var v struct {
		Schema int `json:"schema"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil // Decode reports malformed JSON
	}
	switch {
	case v.Schema < 0:
		return fmt.Errorf("schema %d is not valid", v.Schema)
	case v.Schema > overlaySchema:
		return fmt.Errorf("schema %d is newer than this build", v.Schema)
	}
	return nil
}

// overlayJSONErr names the key a decode error is about, never its value or Go type names.
func overlayJSONErr(err error) error {
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("not valid JSON at byte %d", se.Offset)
	}
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		key, _, _ := strings.Cut(strings.TrimPrefix(te.Field, "."), ".")
		return fmt.Errorf("%s: want %s", key, keyType(key))
	}
	if _, q, ok := strings.Cut(err.Error(), "json: unknown field "); ok { // encoding/json has no typed error for it
		key, uerr := strconv.Unquote(q)
		if uerr != nil {
			key = q
		}
		keys := slices.Concat([]string{"schema"}, RoutingKeys(), []string{"set", "unset", "keep"})
		return fmt.Errorf("unknown key %s (%s)", key, strings.Join(keys, "|"))
	}
	return err
}

// keyType is the JSON type word a document key wants.
func keyType(key string) string {
	switch key {
	case "schema":
		return "a number"
	case "set":
		return "an object of string values"
	case "unset", "keep":
		return "a list of strings"
	}
	return "a string"
}

// FlagOverlay is the --env/--keep-env shorthand, checked by the same rules.
func FlagOverlay(env, keep []string) (Overlay, error) {
	o := Overlay{Keep: slices.Clone(keep)}
	for _, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if !ok {
			return Overlay{}, errors.New("--env: want NAME=VALUE")
		}
		if o.Set == nil {
			o.Set = map[string]string{}
		}
		o.Set[name] = val
	}
	return o, o.validate(flagSrc)
}

// Empty reports an overlay whose env layer (set, unset, keep) changes nothing.
func (o Overlay) Empty() bool { return len(o.Set)+len(o.Unset)+len(o.Keep) == 0 }

// Routed reports an overlay naming where or how the dispatch runs: any of its seven routing keys.
func (o Overlay) Routed() bool {
	return slices.ContainsFunc(routeKeys, func(k routeKey) bool { return k.get(o) != "" })
}

// Confinement is the internal provider[:profile] value ("databricks:qa", "anthropic"); "" when no provider is named.
func (o Overlay) Confinement() string { return router.JoinCoworker(o.Provider, o.Profile) }

// InlineOverlay is the compact document naming provider (and profile) for an argv relay; "" when provider is "".
func InlineOverlay(provider, profile string) string {
	if provider == "" {
		return ""
	}
	doc, _ := json.Marshal(Overlay{Provider: provider, Profile: profile}) // strings only: Marshal cannot fail
	return string(doc)
}

// ResolveOverlay swaps each @secret ref for its vault value, failing closed; it never reads the process env.
func ResolveOverlay(o Overlay) (Resolved, error) {
	out := Resolved{Set: maps.Clone(o.Set), Unset: slices.Clone(o.Unset), Keep: slices.Clone(o.Keep)}
	refs := map[string]string{} // set name -> vault name
	route := providers.RouteSecretGuard()
	for _, k := range slices.Sorted(maps.Keys(o.Set)) {
		ref, ok := providers.SecretRefName(k, o.Set[k])
		if !ok {
			continue
		}
		if err := setRule(setRules, k, o.Set[k], nil, route); err != nil { // re-checked: o may never have been validated
			return Resolved{}, fmt.Errorf("overlay set %s: %w", k, err)
		}
		refs[k] = ref
	}
	if len(refs) == 0 {
		return out, nil
	}
	names := slices.Sorted(maps.Keys(refs))
	vals, err := vault.ReadSecrets(slices.Collect(maps.Values(refs))) // one View and one key lookup for every ref
	if err != nil {
		return Resolved{}, fmt.Errorf("overlay set %s: @secret:%s: %w", names[0], refs[names[0]], err)
	}
	for _, k := range names {
		s := vals[refs[k]]
		if s == "" {
			return Resolved{}, fmt.Errorf("overlay set %s: @secret:%s is not in the vault", k, refs[k])
		}
		out.Set[k] = s
	}
	return out, nil
}

func (o Overlay) validate(src overlaySrc) error {
	if len(o.Files) > 0 { // present at all: [] and null too
		return errors.New("files: not supported yet")
	}
	if err := o.checkRouting(); err != nil {
		return err
	}
	keep, err := o.checkKeep(src)
	if err != nil {
		return err
	}
	if err := o.checkSet(src, keep); err != nil {
		return err
	}
	return o.checkUnset(src, keep)
}

// ErrRemoteWithHost refuses remote on a run that goes to a host.
var ErrRemoteWithHost = errors.New("remote with a host: drop one (the far machine ignores remote)")

// checkRouting applies routeKeys' rules in order, then the cross-key ones; an error names the key, never its value.
func (o Overlay) checkRouting() error {
	for _, k := range routeKeys {
		if err := k.check(k.get(o)); err != nil {
			return err
		}
	}
	return o.checkCrossKeys()
}

// check is one routing key's own rule for value v; "" is unset and passes.
func (k routeKey) check(v string) error {
	switch {
	case v == "":
	case strings.TrimSpace(v) == "" || strings.ContainsFunc(v, unicode.IsControl):
		return errors.New(k.name + ": blank or holds a control character")
	case k.ok != nil && !k.ok(v):
		return errors.New(k.name + ": " + k.msg)
	}
	return nil
}

// checkCrossKeys applies the rules spanning several routing keys: profile needs provider, remote excludes host.
func (o Overlay) checkCrossKeys() error {
	switch {
	case o.Profile != "" && o.Provider == "":
		return errors.New("profile: needs a provider")
	case strings.ContainsFunc(o.Profile, func(r rune) bool { return r == ':' || unicode.IsSpace(r) }):
		return errors.New("profile: want a name without ':' or spaces")
	case o.Remote != "" && o.Host != "" && !remote.IsSelf(o.Host):
		return ErrRemoteWithHost
	}
	return nil
}

// errEnvName refuses a malformed name; nameErr never echoes it (it may be a pasted token).
var errEnvName = errors.New("want an env-style name (A-Z, 0-9, _)")

func nameErr(label, name string, err error) error {
	if err == errEnvName {
		return fmt.Errorf("%s: %w", label, err)
	}
	return fmt.Errorf("%s %s: %w", label, name, err)
}

func (o Overlay) checkKeep(src overlaySrc) (map[string]bool, error) {
	keep := map[string]bool{} // upper-cased: set, keep and unset names overlap whatever their case
	for _, n := range o.Keep {
		if err := nameRule(keepRules, n); err != nil {
			return nil, nameErr(src.keep, n, err)
		}
		keep[strings.ToUpper(n)] = true
	}
	return keep, nil
}

func (o Overlay) checkSet(src overlaySrc, keep map[string]bool) error {
	route := providers.RouteSecretGuard()
	for _, n := range slices.Sorted(maps.Keys(o.Set)) {
		if err := setRule(src.setRules, n, o.Set[n], keep, route); err != nil {
			return nameErr(src.set, n, err)
		}
	}
	return nil
}

func (o Overlay) checkUnset(src overlaySrc, keep map[string]bool) error {
	taken := maps.Clone(keep)
	for n := range o.Set {
		taken[strings.ToUpper(n)] = true
	}
	for _, n := range o.Unset {
		err := nameRule(unsetRules, n)
		if err == nil && taken[strings.ToUpper(n)] {
			err = errors.New("also set or kept")
		}
		if err != nil {
			return nameErr(src.unset, n, err)
		}
	}
	return nil
}

// credentialName reports a credential-shaped name: AUTH anywhere, or a redaction segment (KEY, TOKEN, PAT, ...).
func credentialName(up string) bool {
	return strings.Contains(up, "AUTH") || providers.IsSecretName(up)
}

// rule is one refusal: the first whose match holds decides the result, even when its err returns nil.
type rule struct {
	match func(c *ruleCtx) bool
	err   func(c *ruleCtx) error
}

// ruleCtx is one name (and for set, its value) under test; up is the upper-cased name.
type ruleCtx struct {
	name, up, val, ref string
	isRef, reserved    bool
	keep               map[string]bool
	route              func(string) bool
}

func refuse(err error) func(*ruleCtx) error { return func(*ruleCtx) error { return err } }

func runRules(rules []rule, c *ruleCtx) error {
	for _, r := range rules {
		if r.match(c) {
			return r.err(c)
		}
	}
	return nil
}

// nameRules are the checks keep and unset names share: a malformed, route-redirecting or config-relocating name.
var nameRules = []rule{
	{func(c *ruleCtx) bool { return !vault.EnvName.MatchString(c.name) }, refuse(errEnvName)},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)},
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
}

var keepRules = slices.Concat(nameRules, []rule{
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, "HARNESS_") || credentialName(c.up) },
		refuse(errors.New("a credential name cannot be kept: the scrub withholds it from every agent child"))}})

// unsetRules refuse dropping a name the launch itself sets for its route: a lost endpoint would keep the credential.
var unsetRules = slices.Concat(nameRules, []rule{
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, "HARNESS_") },
		refuse(errors.New("harness-owned: the launch sets it for its route; use --profile"))}})

func nameRule(rules []rule, name string) error {
	return runRules(rules, &ruleCtx{name: name, up: strings.ToUpper(name)})
}

// flagSetRules are the --env rule set: name checks plus the value's credential shape.
var flagSetRules = []rule{
	{func(c *ruleCtx) bool { return c.name == "" || strings.ContainsAny(c.name, " \t") },
		refuse(errors.New("want NAME=VALUE"))},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)}, // before the scrub check
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
	{func(c *ruleCtx) bool { return envReserved(c.name, c.up) },
		refuse(errors.New("reserved by the harness credential scrub: use `claude-code vault set` or a provider's auth.command"))},
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, OverlayVar) }, // a flag must not point at another overlay
		refuse(errors.New("harness-owned"))},
	{func(c *ruleCtx) bool { return credentialName(c.up) || textutil.LooksSecret(c.val) },
		refuse(errors.New("looks like a credential: store it with `claude-code vault set` or a provider's auth.command, not on the command line"))},
}

var errBaseURL = errors.New("a route endpoint, proxy or TLS setting: pick the endpoint with --profile; " +
	"an override would send the route's credential elsewhere")

var errConfigLocation = errors.New("an agent config, permission or routing setting: the harness sets it per launch; use --profile")

// configLocationName reports a name that injects or relocates an agent's config or route; refused in set, keep and unset.
func configLocationName(up string) bool {
	switch up {
	case "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG", proc.EnvCopilotProvidersConfig, "COPILOT_HOME",
		"COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL", "COPILOT_CUSTOM_INSTRUCTIONS_DIRS",
		"COPILOT_API_URL", "COPILOT_GH_HOST", "CODEX_HOME", proc.EnvClaudeConfigDir, proc.EnvAnthropicCustomHeaders,
		"XDG_CONFIG_HOME", "GEMINI_CLI_HOME":
		return true
	}
	return up == "OPENCODE_PERMISSION" || strings.HasPrefix(up, "OPENCODE_DISABLE_") ||
		strings.HasSuffix(up, "_CONFIG_CONTENT") || strings.HasPrefix(up, proc.CopilotProviderPrefix)
}

// routeRedirectName reports a name that can redirect a route credential or weaken TLS; refused in set, keep and unset.
// DATABRICKS_HOST was allowed by D7 (set+keep); the owner's 2026-11-03 pick supersedes D7 for it.
func routeRedirectName(up string) bool {
	switch up {
	case "DATABRICKS_HOST", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NODE_TLS_REJECT_UNAUTHORIZED",
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "AGY_LLM_GATEWAY_HEADERS":
		return true
	}
	return slices.ContainsFunc(redirectSuffixes, func(s string) bool { return strings.HasSuffix(up, s) })
}

var redirectSuffixes = []string{"_BASE_URL", "_API_BASE", "_ENDPOINT", "_GATEWAY_URL", "_PROXY_URL", "_CA_CERT"}

// envReserved reports a name (as typed or upper-cased) the harness credential scrub withholds.
func envReserved(name, up string) bool { return proc.EnvBlocked(name) || proc.EnvBlocked(up) }

var setRules = []rule{
	{func(c *ruleCtx) bool { return !vault.EnvName.MatchString(c.name) }, refuse(errEnvName)},
	{func(c *ruleCtx) bool {
		return strings.HasPrefix(c.up, "HARNESS_") || strings.HasPrefix(c.up, OverlayVar)
	},
		refuse(errors.New("harness-owned"))},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)},
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
	{func(c *ruleCtx) bool { return c.reserved && c.isRef },
		refuse(errors.New("reserved by the credential scrub: a vault ref cannot replace a route value"))},
	{func(c *ruleCtx) bool { return c.reserved && nameRule(keepRules, c.name) != nil }, // keep refuses it too
		refuse(errors.New("reserved by the credential scrub: no overlay can pass it (keep and @secret refs are refused too)"))},
	{func(c *ruleCtx) bool { return c.reserved && !c.keep[c.up] },
		refuse(errors.New("reserved by the credential scrub: add it to keep to pass your own value"))},
	{func(c *ruleCtx) bool { return c.isRef }, func(c *ruleCtx) error { return refRule(c.ref, c.route) }},
	{func(c *ruleCtx) bool { return strings.ContainsRune(c.val, 0) }, refuse(errors.New("value holds a NUL byte"))},
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.val, providers.SecretRef) },
		refuse(errors.New("malformed @secret reference: want @secret or @secret:<NAME>"))},
	{func(c *ruleCtx) bool { return credentialName(c.up) || textutil.LooksSecret(c.val) },
		refuse(errors.New("looks like a credential: store it with `claude-code vault set`, reference it as @secret:<NAME>"))},
}

// setRule checks one set entry against rules; route reports a profile's route credential (providers.RouteSecretGuard).
func setRule(rules []rule, name, val string, keep map[string]bool, route func(string) bool) error {
	up := strings.ToUpper(name)
	ref, isRef := providers.SecretRefName(name, val)
	c := &ruleCtx{name: name, up: up, val: val, ref: ref, isRef: isRef, keep: keep, route: route,
		reserved: envReserved(name, up)}
	return runRules(rules, c)
}

// refRule refuses a vault name an overlay may not pass on: harness-internal secrets and any profile's route credential.
func refRule(ref string, route func(string) bool) error {
	switch {
	case !vault.EnvName.MatchString(ref):
		return errors.New("want @secret:<NAME> with an env-style NAME")
	case providers.InternalSecretName(ref):
		return fmt.Errorf("@secret:%s is harness-internal", ref)
	case route(ref):
		return fmt.Errorf("@secret:%s is a profile's route credential: use --profile", ref)
	}
	return nil
}

// overlayDebugVar names the 0600 JSONL file RecordOverlayDebug appends to; unset (or no _SALT) records nothing.
const overlayDebugVar = "COWORKER_OVERLAY_DEBUG"

const overlayDebugSaltVar = overlayDebugVar + "_SALT"

// OverlayDebugOn reports whether RecordOverlayDebug writes: COWORKER_OVERLAY_DEBUG and its _SALT are both set.
func OverlayDebugOn() bool {
	path, salt := overlayDebugEnv()
	return path != "" && salt != ""
}

func overlayDebugEnv() (path, salt string) {
	return os.Getenv(overlayDebugVar), os.Getenv(overlayDebugSaltVar)
}

type overlayDebugRecord struct {
	Label string            `json:"label"`
	Agent string            `json:"agent"`
	PID   int               `json:"pid"`
	Names []string          `json:"names"`
	FP    map[string]string `json:"fp"`
}

// EnableOverlayDebug turns recording on for this process and its children (ask --overlay-debug <file>). The path is
// made absolute so nested dispatches write the same file; its salt lives beside it as <file>.salt (0600), reused when
// present so every run appended to one file fingerprints with one salt.
func EnableOverlayDebug(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if os.Getenv(overlayDebugSaltVar) == "" {
		salt, err := overlayDebugSalt(abs + ".salt")
		if err != nil {
			return err
		}
		if err := os.Setenv(overlayDebugSaltVar, salt); err != nil {
			return err
		}
	}
	return os.Setenv(overlayDebugVar, abs)
}

// overlayDebugSalt reads the salt beside the log, or links a fully written one into place (the loser reads the winner's).
func overlayDebugSalt(path string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(b)
	switch err := linkSalt(path, salt); {
	case err == nil:
		return salt, nil
	case !errors.Is(err, os.ErrExist):
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) == 0 {
		return "", errors.Join(err, errors.New("empty salt file "+path))
	}
	return strings.TrimSpace(string(got)), nil
}

// linkSalt writes salt to a temp file and links it to path: atomic, and ErrExist if path exists (never half-written).
func linkSalt(path, salt string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // the temp name only; path keeps the hard link
	_, werr := tmp.WriteString(salt)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

// RecordOverlayDebug appends env's names and HMAC-SHA256 fingerprints (last duplicate wins, as exec does).
func RecordOverlayDebug(agentName string, env []string) {
	path, salt := overlayDebugEnv()
	if path == "" || salt == "" {
		return
	}
	rec := overlayDebugRecord{Label: os.Getenv(overlayDebugVar + "_LABEL"), Agent: agentName, PID: os.Getpid(), FP: map[string]string{}}
	m := hmac.New(sha256.New, []byte(salt)) // one MAC reused across names
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		m.Reset()
		m.Write([]byte(val))
		if _, seen := rec.FP[name]; !seen {
			rec.Names = append(rec.Names, name)
		}
		rec.FP[name] = hex.EncodeToString(m.Sum(nil))[:16]
	}
	slices.Sort(rec.Names)
	line, err := json.Marshal(rec)
	if err == nil {
		err = appendOverlayDebug(path, append(line, '\n'))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: overlay debug:", err)
	}
}

func appendOverlayDebug(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // a pre-existing file keeps its mode on open
		return errors.Join(err, f.Close())
	}
	_, werr := f.Write(line)
	return errors.Join(werr, f.Close())
}
