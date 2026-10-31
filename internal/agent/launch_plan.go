package agent

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// LaunchOptions are the harness flags consumed before passthrough.
type LaunchOptions struct {
	Provider string // the profile name from --profile (field name kept until the agent lane moves off it)
	Model    string // alias, canonical version or wire id, this launch only
	Tier     string // opus|sonnet|haiku|fable: the main-loop model; the other slots keep their own pins
	PrintEnv bool
	Direct   bool     // agents only: skip the proxy, hand the CLI the profile route
	KeepEnv  []string // names the credential scrub must not drop, from --keep-env
	File     string   // codex only: a config.toml whose keys ride as -c overrides after the harness's, from -f
}

// flagValue is the value of the flag at args[i]: val when inline (--flag=val), else the next arg and its index.
func flagValue(args []string, i int, name, val string, hasVal bool) (string, int, error) {
	if hasVal {
		return val, i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s needs a value", name)
	}
	return args[i+1], i + 1, nil
}

// ParseArgs strips harness flags from args; the first "--" ends harness parsing and is dropped.
func ParseArgs(args []string, claude bool) (LaunchOptions, []string, error) {
	var o LaunchOptions
	var rest []string
	if err := databricks.RemovedFlagHint(args); err != nil {
		return o, nil, err
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			return o, append(rest, args[i+1:]...), nil
		case name == "--profile" || name == "--model":
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			if name == "--profile" {
				o.Provider = val
			} else {
				o.Model = val
			}
		case a == "--direct" && !claude:
			o.Direct = true
		case a == "--print-env":
			o.PrintEnv = true
		case (name == "-f" || name == "--file") && !claude:
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			o.File = val
		case name == "--keep-env":
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			o.KeepEnv = append(o.KeepEnv, strings.Split(val, ",")...)
		default:
			rest = append(rest, a)
		}
	}
	return o, rest, nil
}

// Plan is a fully resolved launch: exec Binary with Argv under the base env minus Unset plus Set.
type Plan struct {
	Binary      string
	Argv        []string
	Set         map[string]string
	Unset       []string
	Keep        []string // names the credential scrub must not drop, from LaunchOptions.KeepEnv
	Vendor      bool     // true for a vendor CLI child: HARNESS_VAULT_KEY is scrubbed too (never for Claude itself)
	OverlayPath string
	Overlay     []byte
	Warnings    []string
	OnExecFail  func() // runs when Exec returns (the exec failed): drops state minted for the launch
}

// UsageError is a launch refusal the CLI reports verbatim with exit 2.
type UsageError string

func (e UsageError) Error() string { return string(e) }

// envMap turns a KEY=VALUE env list into a map, dropping malformed entries.
func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Env applies the plan to base (KEY=VALUE list).
func (p Plan) Env(base []string) []string {
	merged, locked := moduleEnv(base)
	return p.envWith(merged, customEnv(), locked)
}

// moduleEnv appends vendorsettings module exports onto base, skipping keys the process env sets; locked names those.
func moduleEnv(base []string) ([]string, map[string]bool) {
	present := map[string]bool{}
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok {
			present[k] = true
		}
	}
	locked := map[string]bool{}
	reg, err := providers.Load()
	if err != nil {
		return base, locked
	}
	out := base
	for _, m := range Modules() {
		for k, v := range m.Env(reg.Routing) {
			if present[k] {
				locked[k] = true // process env already set this module-owned key: protect it from custom env too
				continue
			}
			out = append(out, k+"="+v)
			present[k] = true
		}
	}
	return out, locked
}

// customEnv is the vault env bucket resolved (@secret refs via secrets.Lookup); an unresolved ref is warned, not
// set — a Lookup error (e.g. a locked vault) also leaves the key out of vals, so a base env value survives.
var customEnv = func() map[string]string {
	errored := map[string]bool{}
	vals, missing, err := providers.ResolveCustomEnv(func(k string) (string, bool) {
		v, _, err := secrets.Lookup(k)
		if err != nil {
			fmt.Fprintf(NoticeOut, "claude-code: custom env %s: %v; not set\n", k, err)
			errored[k] = true
			return "", false
		}
		return v, v != ""
	})
	if err != nil {
		fmt.Fprintf(NoticeOut, "claude-code: custom env unreadable: %v\n", err)
	}
	for _, k := range missing {
		if errored[k] {
			continue // already warned above with the real error
		}
		fmt.Fprintf(NoticeOut, "claude-code: custom env %s=@secret has no vault value; not set\n", k)
	}
	return vals
}

// envWith layers base, then custom (user vars), then Set; Unset and Set beat custom, locked keys skip custom.
func (p Plan) envWith(base []string, custom map[string]string, locked map[string]bool) []string {
	unset := map[string]bool{}
	for _, k := range p.Unset {
		unset[k] = true
	}
	merged := maps.Clone(p.Set)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range custom {
		_, harness := p.Set[k]
		if !harness && !unset[k] && !locked[k] {
			merged[k] = v
		}
	}
	p.Set = merged
	return p.plainEnv(base)
}

// plainEnv is the credential scrub, then base minus Unset/Set keys, plus Set.
func (p Plan) plainEnv(base []string) []string {
	if p.Vendor {
		base, _ = proc.ScrubVendor(base, proc.EnvKeepSet(p.Keep))
	} else {
		base, _ = proc.Scrub(base, proc.EnvKeepSet(p.Keep))
	}
	drop := map[string]bool{}
	for _, k := range p.Unset {
		drop[k] = true
	}
	for k := range p.Set {
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(p.Set))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		out = append(out, k+"="+p.Set[k])
	}
	return out
}

// Print writes the resolved launch; header values are masked since static headers may carry secrets.
func (p Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "binary   %s\nargv     %s\n", p.Binary, redactQuoted(strings.Join(p.Argv, " ")))
	for _, k := range p.unsetNames() {
		fmt.Fprintf(w, "unset    %s\n", k)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		v := p.Set[k]
		switch {
		case secretKey(k):
			v = "<redacted>"
		case k == "OPENCODE_CONFIG_CONTENT":
			v = redactQuoted(RedactConfig(v))
		}
		fmt.Fprintf(w, "set      %s=%s\n", k, v)
	}
	for _, k := range slices.Sorted(maps.Keys(providers.CustomEnv())) {
		switch _, harness := p.Set[k]; {
		case harness || slices.Contains(p.Unset, k):
			fmt.Fprintf(w, "custom   %s (overridden by the launch)\n", k)
		default:
			fmt.Fprintf(w, "custom   %s (vault env)\n", k)
		}
	}
	if len(p.Overlay) > 0 {
		fmt.Fprintf(w, "overlay  %s\n%s\n", p.OverlayPath, p.Overlay)
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning  %s\n", warn)
	}
}

// unsetNames is every inherited var this plan drops from the real environment: the fixed Anthropic
// wipe list plus whatever the credential scrub catches, minus anything the plan itself sets.
func (p Plan) unsetNames() []string {
	names := map[string]bool{}
	for _, k := range p.Unset {
		if _, set := p.Set[k]; !set && os.Getenv(k) != "" {
			names[k] = true
		}
	}
	scrub := proc.Scrub
	if p.Vendor {
		scrub = proc.ScrubVendor
	}
	_, dropped := scrub(os.Environ(), proc.EnvKeepSet(p.Keep))
	for _, k := range dropped {
		if _, set := p.Set[k]; !set {
			names[k] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// WriteOverlay writes the --settings overlay the argv names, when the plan has one.
func (p Plan) WriteOverlay() error {
	if len(p.Overlay) == 0 {
		return nil
	}
	return paths.AtomicWrite(p.OverlayPath, p.Overlay, 0o600)
}

// Exec writes the overlay and replaces this process with the target.
func (p Plan) Exec() error {
	if err := p.WriteOverlay(); err != nil {
		p.execFailed()
		return err
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(os.Stderr, "claude-code: warning:", w)
	}
	err := proc.Exec(p.Binary, p.Argv, p.Env(os.Environ()))
	p.execFailed()
	return err
}

func (p Plan) execFailed() {
	if p.OnExecFail != nil {
		p.OnExecFail()
	}
}

func secretKey(k string) bool {
	return secretKeyRE.MatchString(k)
}

// secretKeyRE matches the same credential-shaped name segments as providers' validateEnv.
var secretKeyRE = regexp.MustCompile(`(^|_)(KEY|TOKEN|SECRET|PASSWORD|HEADERS|AUTH|CREDENTIALS?|PAT|BEARER)(_|$)`)

// quotedRE is one double-quoted string; codex -c TOML and opencode JSON carry static provider headers in them.
var quotedRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// redactQuoted masks every credential-shaped quoted string in s.
func redactQuoted(s string) string {
	return quotedRE.ReplaceAllStringFunc(s, func(q string) string {
		inner := q[1 : len(q)-1]
		if shapes.Looks(inner) || shapes.Redact(inner) != inner {
			return `"<redacted>"`
		}
		return q
	})
}

var _ = schemas.Register(schemas.Format{
	Name: "settings-overlay", Owner: "internal/agent", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "overlay-*.json") },
	Version: 1, Keep: "rewritten on every launch; Claude Code reads it, so no schema field",
})
