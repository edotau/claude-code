// overlay.go — COWORKER_OVERLAY: one dispatch's keyed config (routing keys + a set/unset/keep env layer).
package agentconfig

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
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
	if v := cmp.Or(flag, os.Getenv(OverlayVar)); v != "" {
		return v // a nested dispatch inherits $COWORKER_OVERLAY (its confinement) and never reads the vault's
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
		return "an integer"
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
var errEnvName = errors.New("want an env-style name (letters, digits, _; not starting with a digit)")

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

// DispatchOverlayContext resolves DispatchOverlay using the run snapshot.
func DispatchOverlayContext(ctx context.Context, flag string) string {
	if v := cmp.Or(flag, proc.Value(ctx, OverlayVar)); v != "" {
		return v // a nested dispatch inherits $COWORKER_OVERLAY (its confinement) and never reads the vault's
	}
	return strings.TrimSpace(providers.CustomEnv()[OverlayVar])
}

// LoadOverlayContext resolves LoadOverlay using the run snapshot.
func LoadOverlayContext(ctx context.Context, value string, stdin io.Reader) (Overlay, error) {
	value = cmp.Or(value, proc.Value(ctx, OverlayVar))
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
