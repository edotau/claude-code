// overlay.go — COWORKER_OVERLAY: a per-launch {set, unset, keep} layer over a coworker's env; secrets by vault name only.
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

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/vault"
)

// OverlayVar names the JSON overlay file layered over every agent child this process starts.
const OverlayVar = "COWORKER_OVERLAY"

// OverlaySchema versions the overlay document; one without "schema" reads as this version.
const OverlaySchema = 1

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
	Name: "coworker-overlay", Owner: "internal/agentconfig", Kind: schemas.Wire, Version: OverlaySchema,
	New: func() any { return new(Overlay) }, Migrate: schemas.SameShape,
})

type overlaySrc struct {
	set, keep, unset string
	flag             bool // flags keep today's --env value rules; D5 and D7 bind overlay files only
}

var (
	fileSrc     = overlaySrc{"overlay set", "overlay keep", "overlay unset", false}
	flagSrc     = overlaySrc{"--env", "--keep-env", "unset", true}
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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
		if o, err = ParseOverlay(raw); err == nil {
			return o, nil
		}
	}
	return Overlay{}, fmt.Errorf("overlay %s: %w", path, err)
}

func readOverlayFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, errors.New("not a regular file")
	case runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0:
		return nil, errors.New("group- or world-writable: chmod go-w it")
	case info.Size() > overlayMaxBytes:
		return nil, fmt.Errorf("over %d bytes", overlayMaxBytes)
	}
	return io.ReadAll(io.LimitReader(f, overlayMaxBytes+1))
}

// ParseOverlay decodes one document strictly and checks it; errors name keys, never values.
func ParseOverlay(raw []byte) (Overlay, error) {
	var o Overlay
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

// MergeOverlays layers top (the flags) over base: top's set wins per name; keep and unset are unioned and re-checked.
func MergeOverlays(base, top Overlay) (Overlay, error) {
	out := Overlay{Set: maps.Clone(base.Set), Keep: slices.Concat(base.Keep, top.Keep), Unset: slices.Concat(base.Unset, top.Unset)}
	if out.Set == nil && len(top.Set) > 0 {
		out.Set = map[string]string{}
	}
	maps.Copy(out.Set, top.Set)
	return out, out.validate(fileSrc)
}

// Empty reports an overlay that changes nothing.
func (o Overlay) Empty() bool { return len(o.Set)+len(o.Unset)+len(o.Keep) == 0 }

// ResolveOverlay swaps each @secret ref for its vault value, failing closed; it never reads the process env.
func ResolveOverlay(o Overlay) (Resolved, error) {
	out := Resolved{Set: maps.Clone(o.Set), Unset: slices.Clone(o.Unset), Keep: slices.Clone(o.Keep)}
	for k, v := range o.Set {
		ref, ok := providers.SecretRefName(k, v)
		if !ok {
			continue
		}
		s, found, err := vault.ReadSecret(ref)
		switch {
		case err != nil:
			return Resolved{}, fmt.Errorf("overlay set %s: @secret:%s: %w", k, ref, err)
		case !found || s == "":
			return Resolved{}, fmt.Errorf("overlay set %s: @secret:%s is not in the vault", k, ref)
		}
		out.Set[k] = s
	}
	return out, nil
}

func (o Overlay) validate(src overlaySrc) error {
	if o.Schema > OverlaySchema {
		return fmt.Errorf("schema %d is newer than this build", o.Schema)
	}
	if len(o.Files) > 0 {
		return errors.New("overlay files: not supported yet")
	}
	keep := map[string]bool{}
	for _, n := range o.Keep {
		if err := keepRule(n); err != nil {
			return fmt.Errorf("%s %s: %w", src.keep, n, err)
		}
		keep[n] = true
	}
	for _, n := range slices.Sorted(maps.Keys(o.Set)) {
		if err := setRule(n, o.Set[n], keep, src.flag); err != nil {
			return fmt.Errorf("%s %s: %w", src.set, n, err)
		}
	}
	for _, n := range o.Unset {
		if !envNameRE.MatchString(n) {
			return fmt.Errorf("%s: want a NAME", src.unset)
		}
		if _, set := o.Set[n]; set || keep[n] {
			return fmt.Errorf("%s %s: also set or kept", src.unset, n)
		}
	}
	return nil
}

// credentialName is the credential shape --env has always refused, widened by the redaction segments (HEADERS, PAT, ...).
func credentialName(up string) bool {
	return strings.HasSuffix(up, "_API_KEY") || strings.HasSuffix(up, "_TOKEN") || strings.HasSuffix(up, "_SECRET") ||
		strings.Contains(up, "AUTH") || secretKeyRE.MatchString(up)
}

func keepRule(name string) error {
	up := strings.ToUpper(name)
	switch {
	case !envNameRE.MatchString(name):
		return errors.New("want a NAME")
	case strings.HasPrefix(up, "HARNESS_") || credentialName(up):
		return errors.New("a credential name cannot be kept: the scrub withholds it from every agent child")
	}
	return nil
}

// flagSetRule is the --env rule set: name checks only (D6 added), the value is never inspected.
func flagSetRule(name string) error {
	up := strings.ToUpper(name)
	switch {
	case name == "" || strings.ContainsAny(name, " \t"):
		return errors.New("want NAME=VALUE")
	case proc.EnvBlocked(name) || proc.EnvBlocked(up):
		return errors.New("reserved by the harness credential scrub: use `claude-code vault set` or a provider's auth.command")
	case strings.HasPrefix(up, OverlayVar): // a flag must not point a coworker at another overlay or debug file
		return errors.New("harness-owned")
	case strings.HasSuffix(up, "_BASE_URL"):
		return errBaseURL
	case flagCredentialName(up):
		return errors.New("looks like a credential: store it with `claude-code vault set` or a provider's auth.command, not on the command line")
	}
	return nil
}

// flagCredentialName is --env's name-only credential test (today's ValidateEnv): key, token or secret suffix, or AUTH.
func flagCredentialName(up string) bool {
	return strings.HasSuffix(up, "_API_KEY") || strings.HasSuffix(up, "_TOKEN") || strings.HasSuffix(up, "_SECRET") ||
		strings.Contains(up, "AUTH")
}

var errBaseURL = errors.New("a route endpoint: pick it with --profile; an override would send the route's credential there")

func setRule(name, val string, keep map[string]bool, flag bool) error {
	if flag {
		return flagSetRule(name)
	}
	up := strings.ToUpper(name)
	ref, isRef := providers.SecretRefName(name, val)
	reserved := proc.EnvBlocked(name) || proc.EnvBlocked(up)
	switch {
	case !envNameRE.MatchString(name):
		return errors.New("want NAME=VALUE")
	case strings.HasPrefix(up, "HARNESS_") || strings.HasPrefix(name, OverlayVar):
		return errors.New("harness-owned")
	case strings.HasSuffix(up, "_BASE_URL"):
		return errBaseURL
	case reserved && isRef:
		return errors.New("reserved by the credential scrub: a vault ref cannot replace a route value")
	case reserved && !keep[name]:
		return errors.New("reserved by the credential scrub: add it to keep to pass your own value")
	case isRef:
		return refRule(ref)
	case strings.ContainsRune(val, 0):
		return errors.New("value holds a NUL byte")
	case strings.HasPrefix(val, providers.SecretRef):
		return errors.New("malformed @secret reference: want @secret or @secret:<NAME>")
	case credentialName(up) || shapes.Looks(val):
		return errors.New("looks like a credential: store it with `claude-code vault set`, reference it as @secret:<NAME>")
	}
	return nil
}

// refRule refuses a vault name an overlay may not pass on: harness-internal secrets and any profile's route credential.
func refRule(ref string) error {
	switch {
	case !envNameRE.MatchString(ref):
		return errors.New("want @secret:<NAME> with an env-style NAME")
	case providers.InternalSecretName(ref):
		return fmt.Errorf("@secret:%s is harness-internal", ref)
	case routeCredential(ref):
		return fmt.Errorf("@secret:%s is a profile's route credential: use --profile", ref)
	}
	return nil
}

func routeCredential(name string) bool {
	if name == "CLAUDE_CODE_OAUTH_TOKEN" || name == "COPILOT_GITHUB_TOKEN" {
		return true
	}
	reg, err := providers.Load()
	if err != nil {
		return true // fail closed
	}
	for _, p := range reg.Providers {
		if p != nil && slices.Contains(providers.CredentialNames(p), name) {
			return true
		}
	}
	return false
}
