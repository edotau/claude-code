// overlay.go — COWORKER_OVERLAY: a per-launch {set, unset, keep} layer over an agent's env; secrets by vault name only.
package agentconfig

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// OverlayVar names the JSON overlay file layered over every agent child this process starts.
const OverlayVar = "COWORKER_OVERLAY"

// overlaySchema versions the overlay document; one without "schema" reads as this version.
const overlaySchema = 1

const overlayMaxBytes = 64 << 10

// Overlay is one launch's env layer; Files is reserved and refused until a launch writes them.
type Overlay struct {
	Schema int               `json:"schema,omitempty"`
	Set    map[string]string `json:"set,omitempty"`
	Unset  []string          `json:"unset,omitempty"`
	Keep   []string          `json:"keep,omitempty"`
	Files  []json.RawMessage `json:"files,omitempty"`
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

type overlaySrc struct {
	set, keep, unset string
	flag             bool // flags keep today's --env value rules; D5 and D7 bind overlay files only
}

var (
	fileSrc     = overlaySrc{"overlay set", "overlay keep", "overlay unset", false}
	flagSrc     = overlaySrc{"--env", "--keep-env", "unset", true}
	secretKeyRE = regexp.MustCompile(`(^|_)(KEY|TOKEN|SECRET|PASSWORD|HEADERS|AUTH|CREDENTIALS?|PAT|BEARER)(_|$)`)
)

// LoadOverlay reads and checks path, else $COWORKER_OVERLAY; neither set is the empty overlay.
func LoadOverlay(path string) (Overlay, error) {
	path = cmp.Or(path, os.Getenv(OverlayVar))
	if path == "" {
		return Overlay{}, nil
	}
	raw, err := readOverlayFile(path)
	if err == nil {
		var o Overlay
		if o, err = parseOverlay(raw); err == nil {
			return o, nil
		}
	}
	return Overlay{}, fmt.Errorf("overlay %s: %w", path, err)
}

func readOverlayFile(path string) ([]byte, error) {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errNotRegular // before Open: opening a FIFO would block
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := checkOverlayInfo(f.Stat()); err != nil { // the opened file's own mode: no stat-to-read swap
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, overlayMaxBytes+1))
	if err == nil && len(raw) > overlayMaxBytes {
		return nil, fmt.Errorf("over %d bytes", overlayMaxBytes) // grew after the stat
	}
	return raw, err
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
	{func(i os.FileInfo) bool { return i.Size() > overlayMaxBytes }, fmt.Errorf("over %d bytes", overlayMaxBytes)},
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

// overlayJSONErr drops the offending character a syntax error quotes: it may be part of a value.
func overlayJSONErr(err error) error {
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("not valid JSON at byte %d", se.Offset)
	}
	return err
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

// Empty reports an overlay that changes nothing.
func (o Overlay) Empty() bool { return len(o.Set)+len(o.Unset)+len(o.Keep) == 0 }

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
		if err := setRule(k, o.Set[k], nil, false, route); err != nil { // re-checked: o may never have been validated
			return Resolved{}, fmt.Errorf("%s %s: %w", fileSrc.set, k, err)
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
	if len(o.Files) > 0 {
		return errors.New("overlay files: not supported yet")
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

func (o Overlay) checkKeep(src overlaySrc) (map[string]bool, error) {
	keep := map[string]bool{}
	for _, n := range o.Keep {
		if err := keepRule(n); err != nil {
			return nil, fmt.Errorf("%s %s: %w", src.keep, n, err)
		}
		keep[n] = true
	}
	return keep, nil
}

func (o Overlay) checkSet(src overlaySrc, keep map[string]bool) error {
	route := providers.RouteSecretGuard()
	for _, n := range slices.Sorted(maps.Keys(o.Set)) {
		if err := setRule(n, o.Set[n], keep, src.flag, route); err != nil {
			return fmt.Errorf("%s %s: %w", src.set, n, err)
		}
	}
	return nil
}

func (o Overlay) checkUnset(src overlaySrc, keep map[string]bool) error {
	for _, n := range o.Unset {
		if !vault.EnvName.MatchString(n) {
			return fmt.Errorf("%s: want a NAME", src.unset)
		}
		if err := unsetRule(n); err != nil {
			return fmt.Errorf("%s %s: %w", src.unset, n, err)
		}
		if _, set := o.Set[n]; set || keep[n] {
			return fmt.Errorf("%s %s: also set or kept", src.unset, n)
		}
	}
	return nil
}

// unsetRule refuses dropping a name the launch itself sets for its route: a lost endpoint would keep the credential.
func unsetRule(name string) error {
	up := strings.ToUpper(name)
	switch {
	case routeRedirectName(up):
		return errBaseURL
	case configLocationName(up):
		return errConfigLocation
	case strings.HasPrefix(up, "HARNESS_"):
		return errors.New("harness-owned: the launch sets it for its route; use --profile")
	}
	return nil
}

// credentialName is the credential shape --env has always refused, widened by the redaction segments (HEADERS, PAT, ...).
func credentialName(up string) bool {
	return strings.Contains(up, "AUTH") || secretKeyRE.MatchString(up) // KEY/TOKEN/SECRET segments cover the old suffixes
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

var keepRules = []rule{
	{func(c *ruleCtx) bool { return !vault.EnvName.MatchString(c.name) }, refuse(errors.New("want a NAME"))},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)},
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.up, "HARNESS_") || credentialName(c.up) },
		refuse(errors.New("a credential name cannot be kept: the scrub withholds it from every agent child"))},
}

func keepRule(name string) error {
	return runRules(keepRules, &ruleCtx{name: name, up: strings.ToUpper(name)})
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

func flagSetRule(name, val string) error {
	return runRules(flagSetRules, &ruleCtx{name: name, up: strings.ToUpper(name), val: val})
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

// routeRedirectName reports a name that can redirect a route credential or weaken TLS; refused in set and keep.
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
	{func(c *ruleCtx) bool { return !vault.EnvName.MatchString(c.name) }, refuse(errors.New("want NAME=VALUE"))},
	{func(c *ruleCtx) bool {
		return strings.HasPrefix(c.up, "HARNESS_") || strings.HasPrefix(c.up, OverlayVar)
	},
		refuse(errors.New("harness-owned"))},
	{func(c *ruleCtx) bool { return routeRedirectName(c.up) }, refuse(errBaseURL)},
	{func(c *ruleCtx) bool { return configLocationName(c.up) }, refuse(errConfigLocation)},
	{func(c *ruleCtx) bool { return c.reserved && c.isRef },
		refuse(errors.New("reserved by the credential scrub: a vault ref cannot replace a route value"))},
	{func(c *ruleCtx) bool { return c.reserved && !c.keep[c.name] },
		refuse(errors.New("reserved by the credential scrub: add it to keep to pass your own value"))},
	{func(c *ruleCtx) bool { return c.isRef }, func(c *ruleCtx) error { return refRule(c.ref, c.route) }},
	{func(c *ruleCtx) bool { return strings.ContainsRune(c.val, 0) }, refuse(errors.New("value holds a NUL byte"))},
	{func(c *ruleCtx) bool { return strings.HasPrefix(c.val, providers.SecretRef) },
		refuse(errors.New("malformed @secret reference: want @secret or @secret:<NAME>"))},
	{func(c *ruleCtx) bool { return credentialName(c.up) || textutil.LooksSecret(c.val) },
		refuse(errors.New("looks like a credential: store it with `claude-code vault set`, reference it as @secret:<NAME>"))},
}

// setRule checks one set entry; route reports a profile's route credential (providers.RouteSecretGuard).
func setRule(name, val string, keep map[string]bool, flag bool, route func(string) bool) error {
	if flag {
		return flagSetRule(name, val)
	}
	up := strings.ToUpper(name)
	ref, isRef := providers.SecretRefName(name, val)
	c := &ruleCtx{name: name, up: up, val: val, ref: ref, isRef: isRef, keep: keep, route: route,
		reserved: envReserved(name, up)}
	return runRules(setRules, c)
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
