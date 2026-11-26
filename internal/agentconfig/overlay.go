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
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
)

// overlaySchema versions the overlay document; one without "schema" reads as this version.
const overlaySchema = 2

const overlayMaxBytes = 64 << 10

// Overlay is one dispatch's config document: where it runs plus its env layer; Files is reserved and refused.
type Overlay struct {
	Schema int `json:"schema,omitempty"`
	routeUnit
	runKeys
	Model         string            `json:"model,omitempty"`
	Effort        string            `json:"effort,omitempty"`
	Fallback      string            `json:"fallback,omitempty"`
	Runner        string            `json:"runner,omitempty"`
	ProviderMode  map[string]string `json:"providerMode,omitempty"`
	Set           map[string]string `json:"set,omitempty"`
	Unset         []string          `json:"unset,omitempty"`
	Keep          []string          `json:"keep,omitempty"`
	TokenCommand  *[]string         `json:"tokenCommand,omitempty"`
	MaxChildren   *int              `json:"maxChildren,omitempty"`
	MaxTreeTokens *int              `json:"maxTreeTokens,omitempty"`
	MaxDepth      *int              `json:"maxDepth,omitempty"`
	Codemode      string            `json:"codemode,omitempty"`
	Fence         *route.Fence      `json:"fence,omitempty"`
	BudgetUSD     *float64          `json:"budgetUSD,omitempty"`
	BudgetCredits *int              `json:"budgetCredits,omitempty"`
	CodexNetwork  bool              `json:"codexNetwork,omitempty"`
	Cloud         bool              `json:"cloud,omitempty"`
	Files         json.RawMessage   `json:"files,omitempty"`
}

// routeKey is one routing key: its JSON name, the typed flag it folds under ("" = none), its field and its rule.
type routeKey struct {
	name, flag string
	get        func(Overlay) string
	ok         func(v string) bool // nil = any text
	msg        string              // the refusal after "<name>: "; never quotes the value
}

// routeKeys defines every routing key once: validation, Fold and Routed all read this table.
var routeKeys = []routeKey{
	{"provider", "", func(o Overlay) string { return o.Provider }, in(providers.ProviderKeys()),
		"unknown (" + strings.Join(providers.ProviderKeys(), "|") + ")"},
	{"profile", "", func(o Overlay) string { return o.Profile }, nil, ""},
	{"model", "model", func(o Overlay) string { return o.Model }, nil, ""},
	{"effort", "effort", func(o Overlay) string { return o.Effort }, nil, ""},
	{"fallback", "fallback", func(o Overlay) string { return o.Fallback }, nil, ""},
	{"runner", "runner", func(o Overlay) string { return o.Runner }, nil, ""},
	{"host", "host", func(o Overlay) string { return o.Host }, sshHost, "not a valid ssh host"},
	{"remote", "", func(o Overlay) string { return o.Remote }, sshHost, "not a valid ssh host"},
	{"account", "", func(o Overlay) string { return o.Account }, nil, ""},
	{"identity", "", func(o Overlay) string { return o.Identity }, in(identityModes),
		"want " + strings.Join(identityModes, ", ")},
}

// identityModes are the Databricks identity values: the --as pair plus auto (the profile's own setting).
var identityModes = []string{providers.IdentityAuto, providers.IdentityUser, providers.IdentitySP}

func in(names []string) func(string) bool {
	return func(v string) bool { return slices.Contains(names, v) }
}

// Fold sets each setting key (model, effort, fallback, host) on its flag in fs unless typed: flag > overlay > default.
// The route unit is never folded key by key: the caller takes it whole with route.Pick.
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

// readCapped reads r one byte past overlayMaxBytes: LoadOverlayContext's cap sees an oversized document, unread.
func readCapped(r io.Reader) ([]byte, error) { return io.ReadAll(io.LimitReader(r, overlayMaxBytes+1)) }

// ReadJSONSource reads value as stdin for -, inline JSON for {...}, else a private regular file; label names errors.
func ReadJSONSource(label, value string, stdin io.Reader) ([]byte, error) {
	if value != "-" && !strings.HasPrefix(strings.TrimSpace(value), "{") {
		if fi, err := os.Lstat(value); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s %s: symlink refused", label, value)
		}
	}
	where, raw, err := readJSONSource(label, value, stdin)
	if err == nil && len(raw) > overlayMaxBytes {
		err = fmt.Errorf("over %d bytes", overlayMaxBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return raw, nil
}

// presetDoc is the overlay a preset names: "coworker" (the Databricks arm) or a bare provider; routing keys only.
func presetDoc(value string) (string, bool) {
	if value = strings.TrimSpace(value); value == "coworker" {
		value = router.RouteDatabricks
	}
	if !slices.Contains(providers.ProviderKeys(), value) {
		return "", false
	}
	return Overlay{Provider: value, Profile: route.AutoArm}.Doc(), true // a provider needs a profile: auto picks
}

// readOverlaySource returns value's error label and bytes: a preset, stdin for -, inline JSON, else the file it names.
func readOverlaySource(value string, stdin io.Reader) (string, []byte, error) {
	if _, err := os.Lstat(value); errors.Is(err, fs.ErrNotExist) { // a file by that name, even unreadable, still wins
		if doc, ok := presetDoc(value); ok {
			return "overlay", []byte(doc), nil
		}
		if p, _, hasColon := strings.Cut(strings.TrimSpace(value), ":"); hasColon && slices.Contains(providers.ProviderKeys(), p) {
			return "overlay", nil, errors.New("provider:profile colon form is removed; use JSON {\"provider\":...,\"profile\":...}")
		}
	}
	return readJSONSource("overlay", value, stdin)
}

func readJSONSource(where, value string, stdin io.Reader) (string, []byte, error) {
	raw := []byte(strings.TrimSpace(value))
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
	case errors.Is(err, fs.ErrNotExist) &&
		(strings.ContainsAny(path, "/"+string(os.PathSeparator)) || strings.HasSuffix(path, ".json")):
		return nil, errNoSuchFile
	case err != nil:
		return nil, errNoOverlay
	case !info.Mode().IsRegular():
		return nil, errNotRegular // before Open: opening a FIFO would block
	}
	f, err := os.Open(path)
	if err != nil {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			return nil, pe.Err // LoadOverlayContext names the path once
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
func parseOverlay(raw []byte) (Overlay, error) { return parseOverlayFrom(raw, "typed", false) }

func parseOverlayFrom(raw []byte, origin string, stored bool) (Overlay, error) {
	raw = forgivingLimits(raw)
	if stored {
		raw = dropStoredUnknown(raw, origin)
	}
	var o Overlay
	if err := cmp.Or(checkOverlaySchema(raw), checkEmptyRun(raw)); err != nil {
		return Overlay{}, err
	}
	if _, err := schemas.Decode(overlayFormat, raw, &o); err != nil {
		return Overlay{}, overlayJSONErr(err, origin)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(new(Overlay)); err != nil {
		return Overlay{}, overlayJSONErr(err, origin)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Overlay{}, errors.New("trailing data after the JSON object")
	}
	if err := providers.CheckModelShared(o.Model); err != nil { // the model key is a plain id; the route keys carry the row
		return Overlay{}, err
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
func overlayJSONErr(err error, origin string) error {
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("not valid JSON at byte %d", se.Offset)
	}
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		key, _, _ := strings.Cut(strings.TrimPrefix(te.Field, "."), ".")
		return fmt.Errorf("%s: want %s", key, keyType(key))
	}
	if key, ok := unknownFieldKey(err); ok {
		reportUnknownKey([]string{key}, origin)
		return route.UnknownKey(key, overlayNames())
	}
	return err
}

func keyType(key string) string {
	switch key {
	case "budgetCredits":
		return "an integer >= 30"
	case "budgetUSD":
		return "a number > 0"
	}
	for _, f := range overlayFields {
		if f.name != key {
			continue
		}
		switch f.kind {
		case kindInt:
			return "an integer"
		case kindNumber:
			return "a number"
		case kindBool:
			return "true or false"
		case kindStrings:
			return "an object of string values"
		case kindList:
			return "a list of strings"
		case kindFence:
			return "an object with profiles and open"
		case kindObject:
			return "an object"
		}
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

// Empty reports an overlay whose environment and execution controls change nothing.
func (o Overlay) Empty() bool {
	return len(o.Set)+len(o.Unset)+len(o.Keep) == 0 && !o.CodexNetwork && !o.Cloud &&
		o.MaxChildren == nil && o.MaxTreeTokens == nil && o.MaxDepth == nil && o.Codemode == "" && o.Fence == nil &&
		o.BudgetUSD == nil && o.BudgetCredits == nil && o.isZero()
}

// Routed reports an overlay naming where or how the dispatch runs: any of its routing keys.
func (o Overlay) Routed() bool {
	if o.TokenCommand != nil || o.CodexNetwork || o.Cloud || o.BudgetUSD != nil || o.BudgetCredits != nil ||
		len(o.ProviderMode) > 0 {
		return true
	}
	return slices.ContainsFunc(routeKeys, func(k routeKey) bool { return k.get(o) != "" })
}

// Confinement is the provider and profile the overlay confines to; zero when no provider is named.
func (o Overlay) Confinement() route.Route {
	if o.Provider == "" {
		return route.Route{}
	}
	return route.Route{Provider: o.Provider, Profile: o.Profile}
}

// Doc is o as its one-line JSON document; "" when o sets nothing.
func (o Overlay) Doc() string {
	doc, _ := json.Marshal(o) // Overlay holds plain values: Marshal cannot fail.
	if string(doc) == "{}" {
		return ""
	}
	return string(doc)
}

// Hop is the step a derived overlay takes; each hop lets a different subset of keys cross.
type Hop int

const (
	HopAppend        Hop = iota // typed keys onto a base
	HopSessionChild             // a session's dispatch: no fence of its own
	HopDispatchChild            // a leaf's dispatch: the leaf's unit becomes the fence
	HopResume                   // a saved launch overlay replayed whole; only a typed key overrides
	HopFallback                 // the next plan hop: the unit moves inside the fence, a moved unit drops the model
)

// Derive is base copied for hop with add laid over it; neither argument is changed and nothing is read from the env.
func (base Overlay) Derive(add Overlay, hop Hop) (Overlay, error) {
	parent := base
	switch hop {
	case HopAppend, HopResume, HopFallback:
	case HopSessionChild, HopDispatchChild:
		base = base.childCopy(hop)
	default:
		return Overlay{}, fmt.Errorf("unknown hop %d", hop)
	}
	out, add := base.clone(), add.clone()
	if u := add.routeUnit; u.Endpoint != "" || u.Provider != "" || u.Profile != "" || u.Identity != "" || u.Account != "" {
		out.Endpoint, out.Provider, out.Profile, out.Identity, out.Account = u.Endpoint, u.Provider, u.Profile, u.Identity, u.Account
	}
	if hop == HopFallback && add.Model == "" && (out.Provider != base.Provider || out.Profile != base.Profile) {
		out.Model = ""
	}
	out.Host, out.Remote = cmp.Or(add.Host, out.Host), cmp.Or(add.Remote, out.Remote)
	out.Model, out.Effort = cmp.Or(add.Model, out.Model), cmp.Or(add.Effort, out.Effort)
	out.Fallback, out.Runner = cmp.Or(add.Fallback, out.Fallback), cmp.Or(add.Runner, out.Runner)
	out.Codemode = cmp.Or(add.Codemode, out.Codemode)
	out.mergeRun(add, hop)
	out.MaxChildren, out.MaxTreeTokens, out.MaxDepth = smaller(out.MaxChildren, add.MaxChildren),
		smaller(out.MaxTreeTokens, add.MaxTreeTokens), smaller(out.MaxDepth, add.MaxDepth)
	out.BudgetUSD = budget(out.BudgetUSD, add.BudgetUSD, parent.BudgetUSD, hop)
	out.BudgetCredits = budget(out.BudgetCredits, add.BudgetCredits, parent.BudgetCredits, hop)
	out.TokenCommand = cmp.Or(add.TokenCommand, out.TokenCommand)
	out.ProviderMode = mergeMaps(out.ProviderMode, add.ProviderMode)
	out.Set = mergeMaps(out.Set, add.Set)
	out.reconcileEnv(add)
	fence := out.Fence
	if add.Fence != nil {
		narrowed := route.Fence{Open: true}
		if fence != nil {
			narrowed = *fence
		}
		narrowed = narrowed.Narrow(*add.Fence)
		fence = &narrowed
	}
	if fence != nil && !fence.Open && base.Provider != "" && out.Provider != base.Provider {
		return Overlay{}, errors.New("provider: outside the fence (a fenced overlay keeps its provider)")
	}
	if fence != nil && !fence.Open && (out.Provider != "" || out.Profile != "") && !fence.Allows(out.Profile) {
		return Overlay{}, fmt.Errorf("profile %s is outside the fence (%s)", out.Profile, strings.Join(fence.Profiles, ","))
	}
	out.Fence = fence
	if err := out.validate(fileSrc); err != nil {
		return Overlay{}, err
	}
	return out, nil
}

// childCopy is the subset of o a child inherits (D5): unit, account, fence, caps, fallback, runner, mode, restrictions.
func (o Overlay) childCopy(hop Hop) Overlay {
	out := Overlay{
		Schema: overlaySchema, Fallback: o.Fallback, Runner: o.Runner, Codemode: o.Codemode, Fence: clonePtr(o.Fence),
		MaxChildren: clonePtr(o.MaxChildren), MaxTreeTokens: clonePtr(o.MaxTreeTokens), MaxDepth: clonePtr(o.MaxDepth),
	}
	out.Endpoint, out.Provider, out.Profile, out.Identity, out.Account = o.Endpoint, o.Provider, o.Profile, o.Identity, o.Account
	out.runKeys = o.restrictions()
	if hop == HopDispatchChild && o.Profile != "" && o.Profile != route.AutoArm { // the leaf's own profile is its fence
		leaf := route.Fence{Profiles: []string{o.Profile}}
		if out.Fence != nil {
			leaf = out.Fence.Narrow(leaf)
		}
		out.Fence = &leaf
	}
	return out
}

// clone is o sharing no map, slice or pointer with it.
func (o Overlay) clone() Overlay {
	o.ProviderMode, o.Set = maps.Clone(o.ProviderMode), maps.Clone(o.Set)
	o.Unset, o.Keep, o.Files = slices.Clone(o.Unset), slices.Clone(o.Keep), slices.Clone(o.Files)
	o.detach()
	o.TokenCommand = clonePtr(o.TokenCommand)
	if o.TokenCommand != nil {
		*o.TokenCommand = slices.Clone(*o.TokenCommand)
	}
	o.MaxChildren, o.MaxTreeTokens, o.MaxDepth = clonePtr(o.MaxChildren), clonePtr(o.MaxTreeTokens), clonePtr(o.MaxDepth)
	o.BudgetUSD, o.BudgetCredits = clonePtr(o.BudgetUSD), clonePtr(o.BudgetCredits)
	if o.Fence != nil {
		o.Fence = &route.Fence{Profiles: slices.Clone(o.Fence.Profiles), Open: o.Fence.Open}
	}
	return o
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// budget is add over cur: a typed append or resume replaces it, a child or fallback never exceeds the parent's limit.
func budget[T cmp.Ordered](cur, add, limit *T, hop Hop) *T {
	switch {
	case add == nil:
		return cur
	case hop != HopAppend && hop != HopResume && limit != nil && *limit < *add:
		return clonePtr(limit)
	}
	return add
}

// smaller is the tighter of two optional caps: nil or negative is unset, 0 is no cap and loses to any positive one.
func smaller(a, b *int) *int {
	switch {
	case a == nil || *a < 0:
		return clonePtr(b)
	case b == nil || *b < 0 || *b == 0:
		return clonePtr(a)
	case *a == 0 || *b < *a:
		return clonePtr(b)
	}
	return clonePtr(a)
}

func mergeMaps(base, add map[string]string) map[string]string {
	if len(base)+len(add) == 0 {
		return nil
	}
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, add)
	return out
}

// reconcileEnv unions add's unset and keep names into o; add wins a collision, names compared case-insensitively.
func (o *Overlay) reconcileEnv(add Overlay) {
	names := func(l []string, m map[string]string) map[string]bool {
		out := map[string]bool{}
		for _, n := range slices.Concat(l, slices.Collect(maps.Keys(m))) {
			out[strings.ToUpper(n)] = true
		}
		return out
	}
	unsets, taken := names(add.Unset, nil), names(add.Keep, add.Set)
	retyped := names(nil, add.Set)
	for n := range o.Set {
		if _, typed := add.Set[n]; !typed && retyped[strings.ToUpper(n)] {
			delete(o.Set, n) // a case-variant of a typed name: the typed one wins
		}
	}
	for n := range o.Set {
		if unsets[strings.ToUpper(n)] {
			delete(o.Set, n)
		}
	}
	o.Keep = slices.DeleteFunc(o.Keep, func(n string) bool { return unsets[strings.ToUpper(n)] })
	o.Unset = slices.DeleteFunc(o.Unset, func(n string) bool { return taken[strings.ToUpper(n)] })
	o.Unset = slices.Compact(slices.Sorted(slices.Values(slices.Concat(o.Unset, add.Unset))))
	o.Keep = slices.Compact(slices.Sorted(slices.Values(slices.Concat(o.Keep, add.Keep))))
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
	if o.TokenCommand != nil && len(*o.TokenCommand) > 0 && strings.TrimSpace((*o.TokenCommand)[0]) == "" {
		return errors.New("tokenCommand: needs an executable")
	}
	if len(o.Files) > 0 { // present at all: [] and null too
		return errors.New("files: not supported yet")
	}
	if err := cmp.Or(o.checkRouting(), o.checkProviderMode(), o.checkRun()); err != nil {
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

// checkProviderMode wants each key a Claude provider of the registry (when it loads) and each value a mode.
func (o Overlay) checkProviderMode() error {
	if len(o.ProviderMode) == 0 {
		return nil
	}
	reg, err := providers.LoadShared()
	if err != nil {
		return errors.New("providerMode: the provider registry does not load, so its providers cannot be checked")
	}
	var names []string
	for _, p := range router.ClaudeProviders(reg) {
		names = append(names, p.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(o.ProviderMode)) {
		if !slices.Contains(names, name) {
			return errors.New("providerMode: unknown provider (" + strings.Join(names, "|") + ")")
		}
		if _, err := router.ParseProviderMode(o.ProviderMode[name]); err != nil || o.ProviderMode[name] == "" {
			return errors.New("providerMode: " + router.ProviderModeError{}.Error()) // empty is no mode: the schema agrees
		}
	}
	return nil
}

// ErrRemoteWithHost refuses remote on a run that goes to a host.
var ErrRemoteWithHost = errors.New("remote with a host: drop one (the far machine ignores remote)")

// checkRouting applies routeKeys' rules in order, then the cross-key ones; an error names the key, never its value.
func (o Overlay) checkRouting() error {
	if err := o.validateBudgetValues(); err != nil {
		return err
	}
	for _, k := range routeKeys {
		if err := k.check(k.get(o)); err != nil {
			return err
		}
	}
	return cmp.Or(o.Settings().Validate(), o.checkCrossKeys())
}

// Settings is the overlay's shared-shape fields as route.AgentSettings; route.AgentSettings.Validate checks the enums.
func (o Overlay) Settings() route.AgentSettings {
	s := route.AgentSettings{Model: o.Model, Effort: o.Effort, FallbackMode: o.Fallback, Runner: o.Runner}
	if !o.IsZero() {
		s.Route = &o.routeUnit
	}
	return s
}

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

// checkCrossKeys applies the rules spanning several routing keys: a route names provider and profile together.
func (o Overlay) checkCrossKeys() error {
	if err := cmp.Or(o.checkRouteUnit(), o.checkRouteScope()); err != nil {
		return err
	}
	switch {
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

// DispatchOverlayContext is a dispatch's overlay: flag, else the run's $COWORKER_OVERLAY, else the vault env's.
func DispatchOverlayContext(ctx context.Context, flag string) string {
	if v := cmp.Or(flag, proc.Value(ctx, route.OverlayVar)); v != "" {
		return v // a nested dispatch inherits $COWORKER_OVERLAY (its confinement) and never reads the vault's
	}
	if values, ok := proc.ValuesFromContext(ctx); ok {
		if _, held := values[route.OverlayVar]; held {
			return ""
		}
	}
	return strings.TrimSpace(providers.CustomEnv()[route.OverlayVar])
}

// LoadOverlayContext reads value, else the run's $COWORKER_OVERLAY: a file, inline JSON or - for stdin; "" is none.
func LoadOverlayContext(ctx context.Context, value string, stdin io.Reader) (Overlay, error) {
	return LoadOverlayFrom(ctx, value, stdin, value != "")
}

// LoadOverlayFrom is LoadOverlayContext with the caller stating whether the user typed the overlay or it is stored.
func LoadOverlayFrom(ctx context.Context, value string, stdin io.Reader, typed bool) (Overlay, error) {
	origin, stored := "typed", !typed
	if stored {
		origin = "env " + route.OverlayVar
	}
	value = cmp.Or(value, proc.Value(ctx, route.OverlayVar))
	if value == "" {
		return Overlay{}, nil
	}
	where, raw, err := readOverlaySource(value, stdin)
	if err == nil && len(raw) > overlayMaxBytes { // every form: file, stdin, inline and env
		err = fmt.Errorf("over %d bytes", overlayMaxBytes)
	}
	var o Overlay
	if err == nil {
		o, err = parseOverlayFrom(raw, origin, stored)
	}
	if err != nil {
		return Overlay{}, fmt.Errorf("%s: %w", where, err)
	}
	return o, nil
}

// CommandProvider copies p with the dispatch's provider command override where present.
func CommandProvider(ctx context.Context, p *providers.Provider) (*providers.Provider, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	o, err := LoadOverlayContext(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	if o.TokenCommand == nil || o.Provider != "" && o.Provider != p.Provider ||
		o.Profile != "" && o.Profile != "auto" && o.Profile != p.Name {
		return p, nil
	}
	out := *p
	out.TokenCommand = slices.Clone(*o.TokenCommand)
	return &out, nil
}

func forgivingLimits(raw []byte) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	for _, key := range []string{"maxChildren", "maxTreeTokens", "maxDepth"} {
		if value, ok := doc[key]; ok {
			var n int
			if json.Unmarshal(value, &n) != nil {
				doc[key] = json.RawMessage("-1")
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// Limit keeps malformed optional caps from refusing a dispatch.
func (o Overlay) Limit(key string, fallback int) int {
	var n *int
	switch key {
	case "HARNESS_DISPATCH_MAX_CHILDREN":
		n = o.MaxChildren
	case "HARNESS_DISPATCH_MAX_TREE_TOKENS":
		n = o.MaxTreeTokens
	case "HARNESS_DISPATCH_MAX_DEPTH":
		n = o.MaxDepth
	}
	if n == nil || *n < 0 {
		return fallback
	}
	return *n
}

func (o Overlay) ValidateBudgets(agent string) error {
	if o.BudgetUSD != nil && agent != "claude" {
		return errors.New("budgetUSD applies to claude only")
	}
	if o.BudgetCredits != nil && agent != "copilot" {
		return errors.New("budgetCredits applies to copilot only")
	}
	return nil
}

func (o Overlay) validateBudgetValues() error {
	if o.BudgetUSD != nil && *o.BudgetUSD <= 0 {
		return errors.New("budgetUSD: want a number > 0")
	}
	if o.BudgetCredits != nil && *o.BudgetCredits < 30 {
		return errors.New("budgetCredits: want an integer >= 30")
	}
	return o.validateRunKeys()
}

// validateRunKeys checks codemode and fence; an error names the key, never its value.
func (o Overlay) validateRunKeys() error {
	switch {
	case o.Codemode != "" && !slices.Contains([]string{"off", "on", "only"}, o.Codemode):
		return errors.New("codemode: want off, on or only")
	case o.Fence != nil && !o.Fence.Open && (o.Provider != "" || o.Profile != "") && !o.Fence.Allows(o.Profile):
		return errors.New("profile: outside the fence")
	case o.Fence != nil && slices.ContainsFunc(o.Fence.Profiles, func(p string) bool {
		return p == "" || strings.ContainsFunc(p, func(r rune) bool { return r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) })
	}):
		return errors.New("fence: profiles want names without ':', spaces or control characters")
	}
	return nil
}

// routeUnit is route.Route embedded flat, so its keys keep their wire names and promote onto Overlay.
type routeUnit = route.Route

// Route is the overlay's route unit, taken whole by route.Pick.
func (o Overlay) Route() route.Route { return o.routeUnit }

// WithoutRoute is o with the route unit's provider, profile, identity and account cleared; host and remote stay.
func (o Overlay) WithoutRoute() Overlay {
	o.Endpoint, o.Provider, o.Profile, o.Identity, o.Account = "", "", "", "", ""
	return o
}

// checkRouteUnit refuses a partial route: a profile, identity or account needs its provider, a provider its profile.
func (o Overlay) checkRouteUnit() error {
	switch {
	case o.Provider == "" && (o.Endpoint != "" || o.Profile != "" || o.Identity != "" || o.Account != ""):
		return errors.New("provider: required with profile, endpoint, identity or account")
	case o.Provider != "" && o.Profile == "":
		return errors.New("profile: required with provider (a registry row, or auto)")
	case o.Endpoint != "":
		return route.Route{Endpoint: o.Endpoint, Provider: o.Provider, Profile: o.Profile}.Validate()
	}
	return nil
}

// checkRouteScope refuses identity off Databricks and account off GitHub.
func (o Overlay) checkRouteScope() error {
	switch {
	case o.Identity != "" && o.Provider != "databricks":
		return errors.New("identity: databricks only")
	case o.Account != "" && o.Provider != "github":
		return errors.New("account: github only")
	}
	return nil
}
