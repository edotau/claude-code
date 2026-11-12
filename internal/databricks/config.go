package databricks

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// cfgFlags are the flags `config` and `rescue` share.
type cfgFlags struct {
	profile, host, wire, auth, out, model string
	force                                 bool
	set, unset                            multi
}

var configPage = helptext.Page{
	Usage: "claude-code databricks config <claude|codex> [flags]",
	About: "Render a ready-to-use Databricks config from embedded templates. " +
		"Prints to stdout (never a token); --out writes it 0600.",
}

var rescuePage = helptext.Page{
	Usage: "claude-code databricks rescue [flags] [-- claude args...]",
	About: "Break-glass: run claude with a scratch settings file that layers on top of " +
		"your user settings; no router and no harness overlay. The file is deleted when claude exits. " +
		"--print shows the file and command only and leaves the file.",
}

func bind(fs *flag.FlagSet, f *cfgFlags, authDef string, withOut bool) {
	profileFlagVar(fs, &f.profile, profileUsage)
	fs.StringVar(&f.host, "host", "", "https://host override, skips the vault")
	fs.StringVar(&f.wire, "wire", "", "ai-gateway or serving (default: the row's, else serving)")
	fs.StringVar(&f.model, "model", "", "model override, canonical or wire id")
	fs.StringVar(&f.auth, "auth", authDef, "harness (token helper) or cli (databricks CLI)")
	fs.Var(&f.set, "set", "K=V env key to add or change (repeatable)")
	fs.Var(&f.unset, "unset", "K env key to remove (repeatable)")
	if withOut {
		fs.StringVar(&f.out, "out", "", "write to FILE (0600) instead of stdout")
		fs.BoolVar(&f.force, "force", false, "overwrite an existing --out file")
	}
}

// resolve returns the provider row and https host: --host builds a preset row, else the registry row's.
func resolve(f cfgFlags) (*providers.Provider, string, error) {
	reg, lerr := providers.Load() // nil reg: --host alone still renders, under the generic name
	name := resolveProfile(reg, f.profile)
	if name == "" {
		name = providers.GroupDatabricks
	}
	if err := checkWire(f.wire); err != nil {
		return nil, "", err
	}
	host, err := flagHost(f.host)
	if err != nil {
		return nil, "", err
	}
	var row *providers.Provider
	if host == "" {
		if row, host, err = registryHost(reg, lerr, name); err != nil {
			return nil, "", err
		}
	}
	if row != nil && rowServesWire(row, f.wire) {
		return row, host, nil
	}
	return synth(name, host, f.wire)
}

// checkWire accepts an empty --wire or one of the two wires.
func checkWire(wire string) error {
	if wire != "" && wire != providers.WireServing && wire != providers.WireAIGateway {
		return fmt.Errorf("--wire must be ai-gateway or serving (got %q)", wire)
	}
	return nil
}

// flagHost normalizes a --host value; "" stays "" (use the registry).
func flagHost(h string) (string, error) {
	if h == "" {
		return "", nil
	}
	if n := oauth.NormalizeHost(h); n != "" {
		return n, nil
	}
	return "", fmt.Errorf("host %q is not a valid https workspace URL", h)
}

// registryHost returns the registry row for name and its https workspace host.
func registryHost(reg *providers.Registry, lerr error, name string) (*providers.Provider, string, error) {
	if reg == nil {
		return nil, "", fmt.Errorf("cannot read the registry (%v); pass --host URL", lerr)
	}
	row := reg.Providers[name]
	if row == nil {
		return nil, "", fmt.Errorf("no provider %q; pass --host URL", name)
	}
	for _, raw := range []string{row.Host, row.BaseURL} {
		if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && u.Host != "" {
			return row, "https://" + u.Host, nil
		}
	}
	return nil, "", fmt.Errorf("provider %q has no host; pass --host URL", name)
}

// rowServesWire reports whether the registry row already uses the requested wire (empty means any).
func rowServesWire(row *providers.Provider, wire string) bool {
	return wire == "" || wire == row.Options.Wire || (wire == providers.WireServing && row.Options.Wire == "")
}

// synth expands a bare databricks preset row for host and wire through the registry's own loader.
func synth(name, host, wire string) (*providers.Provider, string, error) {
	doc, err := json.Marshal(map[string]any{"providers": map[string]any{providers.GroupDatabricks: map[string]any{name: map[string]any{
		"preset": providers.PresetDatabricks, "host": host, "options": map[string]string{"wire": wire}}}}})
	if err != nil {
		return nil, "", err
	}
	reg, _ := providers.Parse([]byte("{}"), doc) // a reg is always returned; the row check below is the gate
	p := reg.Providers[name]
	if p == nil || p.BaseURL == "" {
		return nil, "", fmt.Errorf("host %q is not a valid https workspace URL", host)
	}
	return p, host, nil
}

func (f cfgFlags) options(target, self string) (renderOptions, error) {
	p, host, err := resolve(f)
	if err != nil {
		return renderOptions{}, err
	}
	return renderOptions{Target: target, Host: host, P: p, Auth: f.auth, Self: self,
		Model: f.model, Set: f.set, Unset: f.unset}, nil
}

// config is `databricks config <claude|codex>`.
func config(args []string) int {
	if len(args) == 0 || helptext.WantsHelp(args[:1]) {
		configUsage()
		return 0
	}
	target := args[0]
	var f cfgFlags
	fs := helptext.NewFlagSet("claude-code databricks config", configPage)
	bind(fs, &f, authHarness, true)
	if code, done := helptext.Parse(fs, args[1:]); done {
		return code
	}
	if err := checkProfileFlag(fs, f.profile); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
		return 2
	}
	self, _ := os.Executable()
	o, err := f.options(target, self)
	if err == nil {
		var res renderResult
		if res, err = renderConfig(o); err == nil {
			return emit(f, target, res)
		}
	}
	fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
	return 2
}

func emit(f cfgFlags, target string, res renderResult) int {
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, "note: "+res.Note)
	}
	if f.out == "" {
		_, _ = io.WriteString(os.Stdout, res.Text)
		return 0
	}
	if err := writeFile0600(f.out, res.Text, f.force); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "wrote %s config to %s\n", target, f.out)
	return 0
}

func configUsage() {
	fs := helptext.NewFlagSet("claude-code databricks config", configPage)
	bind(fs, &cfgFlags{}, authHarness, true)
	fs.Usage()
}

// findClaude locates the claude binary: the harness agent link, else PATH.
func findClaude() (string, error) {
	if p, err := paths.AgentBin("claude"); err == nil {
		return p, nil
	}
	return exec.LookPath("claude")
}

// scrubPrefixes and scrubNames are the inherited vars rescue drops so only the scratch settings steer claude.
var (
	scrubPrefixes = []string{"HARNESS_", "ANTHROPIC_", "CLAUDE_CODE_USE_", "DATABRICKS_"}
	scrubNames    = []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}
)

// scrubbed drops proc.ScrubDeny plus every inherited var matching scrubPrefixes or scrubNames.
func scrubbed(environ []string) []string {
	environ, _ = proc.Scrub(environ, nil)
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(scrubNames, name) || slices.ContainsFunc(scrubPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// rescue is `databricks rescue`: scratch settings file, then claude --settings, no router and no overlay.
func rescue(args []string) int {
	if len(args) > 0 && helptext.WantsHelp(args[:1]) {
		rescueUsage()
		return 0
	}
	var f cfgFlags
	var print bool
	fs := helptext.NewFlagSet("claude-code databricks rescue", rescuePage)
	bind(fs, &f, authCLI, false)
	fs.BoolVar(&print, "print", false, "print the file and command, do not run")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	res, file, err := renderScratch(fs, f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: %v\n", err)
		return 2
	}
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, "note: "+res.Note)
	}
	argv := append([]string{"--settings", file}, fs.Args()...)
	bin, ferr := findClaude()
	if print {
		fmt.Printf("# %s\n%s\n# command\nclaude %s\n", file, res.Text, strings.Join(argv, " "))
		return 0
	}
	if ferr != nil {
		_ = os.Remove(file)
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: claude not found: %v\n", ferr)
		return 2
	}
	fmt.Fprintf(os.Stderr, "running: %s %s\n", bin, strings.Join(argv, " "))
	code, err := spawn(bin, argv, scrubbed(os.Environ()), os.Stdin, os.Stdout, os.Stderr)
	_ = os.Remove(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: exec failed: %v\n", err)
		return 2
	}
	return code
}

// renderScratch renders the claude config for f and writes it to a scratch file.
func renderScratch(fs *flag.FlagSet, f cfgFlags) (renderResult, string, error) {
	if err := checkProfileFlag(fs, f.profile); err != nil {
		return renderResult{}, "", err
	}
	self, _ := os.Executable()
	o, err := f.options(targetClaude, self)
	if err != nil {
		return renderResult{}, "", err
	}
	res, err := renderConfig(o)
	if err != nil {
		return renderResult{}, "", err
	}
	file, err := writeScratch(res.Text)
	return res, file, err
}

// writeScratch writes text to a fresh 0600 file under the OS temp dir.
func writeScratch(text string) (string, error) {
	w, err := os.CreateTemp("", "dbx-rescue-*.json")
	if err != nil {
		return "", err
	}
	name := w.Name()
	_, err = w.WriteString(text)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return filepath.Clean(name), nil
}

func rescueUsage() {
	fs := helptext.NewFlagSet("claude-code databricks rescue", rescuePage)
	bind(fs, &cfgFlags{}, authCLI, false)
	fs.Bool("print", false, "print the file and command, do not run")
	fs.Usage()
}
