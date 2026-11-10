package agent

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// NoticeOut is where launch and agent-fallback notices print; tests discard it (also via other packages' TestMain).
var NoticeOut io.Writer = os.Stderr

// ClaudeLaunch is a resolved Claude Code launch: the binary, its argv and env, and the route keys the plan set.
type ClaudeLaunch struct {
	Binary    string
	Argv, Env []string
	Set       map[string]string
}

// ClaudeOptions picks the session route for a Claude launch.
type ClaudeOptions struct {
	Provider, Model string
	KeepEnv         []string
}

// Launcher resolves a Claude launch; the claude package registers the default and tests fake it.
type Launcher interface {
	Claude(ctx context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error)
}

// DefaultLauncher is the Launcher the one Register that supplies it installs; the Client hands it to new runs.
var DefaultLauncher Launcher

// LaunchOptions are the harness flags consumed before passthrough.
type LaunchOptions struct {
	Provider string // the profile name from --profile (field name kept until the agent lane moves off it)
	Model    string // alias, canonical version or wire id, this launch only
	Alias    string // opus|sonnet|haiku|fable: the main-loop model; the other slots keep their own pins
	PrintEnv bool
	Direct   bool     // agents only: skip the proxy, hand the CLI the profile route
	KeepEnv  []string // names the credential scrub must not drop, from --keep-env
	File     string   // codex only: a config.toml whose keys ride as -c overrides after the harness's, from -f
	Overlay  string   // --overlay FILE; empty reads $COWORKER_OVERLAY
}

func (o *LaunchOptions) LaunchFlagSetter(name string, claude bool) func(string) {
	fields := map[string]*string{"--profile": &o.Provider, "--model": &o.Model, "--overlay": &o.Overlay}
	if !claude {
		fields["-f"], fields["--file"] = &o.File, &o.File
	}
	if field := fields[name]; field != nil {
		return func(v string) { *field = v }
	}
	if name == "--keep-env" {
		return func(v string) { o.KeepEnv = append(o.KeepEnv, strings.Split(v, ",")...) }
	}
	return nil
}

// LaunchFile is a 0600 per-launch config the child reads, written by Exec just before the child starts; a file named
// for the child's pid (copilot's registry) is swept once it exits (`claude-code clean`).
type LaunchFile struct {
	Path string
	Data []byte
}

// Plan is a fully resolved launch: exec Binary with Argv under the base env minus Unset plus Set.
type Plan struct {
	Binary      string
	Agent       string // the launched agent, for RecordOverlayDebug at the exec site
	Argv        []string
	Set         map[string]string
	Unset       []string
	Keep        []string     // names the credential scrub must not drop, from LaunchOptions.KeepEnv
	CLI         bool         // true for an agent CLI child: HARNESS_VAULT_KEY is scrubbed too (never for Claude itself)
	Files       []LaunchFile // written 0600 by WriteFiles before the child starts
	Warnings    []string
	ForeignBase bool   // the shell's ANTHROPIC_BASE_URL differs from the profile's: no harness credential may go there
	Profile     string // the profile an agent CLI launch resolved, for RecordLastProfile
	OnExecFail  func() // runs when Exec returns (the exec failed): drops state minted for the launch
}

// Env applies the plan to base (KEY=VALUE list).
func (p Plan) Env(base []string) []string {
	return p.envWith(base, customEnv())
}

// customEnv is the vault env bucket resolved (@secret refs via vault.Lookup); an unresolved ref is warned, not
// set — a Lookup error (e.g. a locked vault) also leaves the key out of vals, so a base env value survives.
var customEnv = func() map[string]string {
	errored := map[string]bool{}
	vals, missing, err := providers.ResolveCustomEnv(func(k string) (string, bool) {
		v, _, err := vault.Lookup(k)
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
		fmt.Fprintf(NoticeOut, "claude-code: custom env %s: its @secret is missing from the vault or not allowed (route or harness secret); not set\n", k)
	}
	return vals
}

// envWith layers base, then custom (user vars), then Set; Unset and Set beat custom.
func (p Plan) envWith(base []string, custom map[string]string) []string {
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
		if !harness && !unset[k] {
			merged[k] = v
		}
	}
	p.Set = merged
	return p.plainEnv(base)
}

// envMode is the deny scrub this plan's child gets: agent CLIs also lose HARNESS_VAULT_KEY.
func (p Plan) envMode() EnvMode {
	if p.CLI {
		return EnvAgent
	}
	return EnvClaude
}

// plainEnv is the plan as an EnvSpec: base scrubbed, minus Unset and Set names, then Set in name order.
func (p Plan) plainEnv(base []string) []string {
	set := make([]string, 0, len(p.Set))
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		set = append(set, k+"="+p.Set[k])
	}
	spec := EnvSpec{Mode: p.envMode(), Base: append([]string{}, base...), Keep: p.Keep, Unset: p.Unset, Set: set}
	return spec.Environ(context.Background()) // interactive: no batch isolation
}

// envRedactors blank the secrets inside a structured env value (key -> redactor), filled by Register from the agent owning the key.
var envRedactors = map[string]func(string) string{}

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
		case envRedactors[k] != nil:
			v = redactQuoted(envRedactors[k](v))
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
	for _, f := range p.Files {
		fmt.Fprintf(w, "overlay  %s\n%s\n", f.Path, redactQuoted(string(f.Data)))
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
	_, dropped := p.envMode().scrub(os.Environ(), p.Keep)
	for _, k := range dropped {
		if _, set := p.Set[k]; !set {
			names[k] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// WriteFiles writes the per-launch files the argv and env name.
func (p Plan) WriteFiles() error {
	for _, f := range p.Files {
		if err := paths.AtomicWrite(f.Path, f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// RecordEnv is the env Exec hands the child, recorded to COWORKER_OVERLAY_DEBUG; --print-env calls it as a dry exec.
func (p Plan) RecordEnv() []string {
	env := p.Env(os.Environ())
	agentconfig.RecordOverlayDebug(p.Agent, env)
	return env
}

// Exec writes the launch files and replaces this process with the target.
func (p Plan) Exec() error {
	if err := p.WriteFiles(); err != nil {
		p.execFailed()
		return err
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(os.Stderr, "claude-code: warning:", w)
	}
	err := proc.Exec(p.Binary, p.Argv, p.RecordEnv())
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
		if textutil.LooksSecret(inner) || textutil.RedactSecrets(inner) != inner {
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

// Folds a resolved COWORKER_OVERLAY (agentconfig) into a headless RunOptions or an interactive Plan.

// OverlayIdle reports a launch the overlay changes nothing about: empty layer and no $COWORKER_OVERLAY to hide.
func OverlayIdle(r agentconfig.Resolved) bool {
	return len(r.Set)+len(r.Unset)+len(r.Keep) == 0 && os.Getenv(agentconfig.OverlayVar) == ""
}

// CheckRouteNames refuses a name in names that route (KEY=VALUE, before the overlay is appended) already sets.
func CheckRouteNames(route, names []string) error {
	own := map[string]bool{}
	for _, kv := range route {
		k, _, _ := strings.Cut(kv, "=")
		own[k] = true
	}
	for _, n := range names {
		if own[n] {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", n)
		}
	}
	return nil
}

// CheckOverlay refuses an overlay that sets or unsets a name the plan's launch sets for its route.
func (p Plan) CheckOverlay(r agentconfig.Resolved) error {
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", k)
		}
	}
	for _, k := range r.Unset {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay unset %s: set by the launch for its route; use --profile", k)
		}
	}
	return nil
}

// WithOverlay layers r over an interactive plan the way ApplyOverlay does for a headless run; the launch's own Set wins.
func (p Plan) WithOverlay(r agentconfig.Resolved) Plan {
	if OverlayIdle(r) {
		return p
	}
	set := maps.Clone(r.Set)
	if set == nil {
		set = map[string]string{}
	}
	maps.Copy(set, p.Set)
	p.Set, p.Keep = set, slices.Concat(p.Keep, r.Keep)
	unset := slices.DeleteFunc(slices.Clone(r.Unset), func(k string) bool { _, own := p.Set[k]; return own })
	p.Unset = slices.Concat(p.Unset, unset, []string{agentconfig.OverlayVar})
	return p
}
