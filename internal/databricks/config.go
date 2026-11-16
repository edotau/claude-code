package databricks

import (
	"context"
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

type cfgFlags struct {
	profile, host, auth, out, model string
	force                           bool
	set, unset                      []string
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
	fs.StringVar(&f.model, "model", "", "model override, canonical or wire id")
	fs.StringVar(&f.auth, "auth", authDef, "harness (token helper) or cli (databricks CLI)")
	helptext.ListVar(fs, &f.set, "set", "value", "K=V env key to add or change (repeatable)", false)
	helptext.ListVar(fs, &f.unset, "unset", "value", "K env key to remove (repeatable)", false)
	if withOut {
		fs.StringVar(&f.out, "out", "", "write to FILE (0600) instead of stdout")
		fs.BoolVar(&f.force, "force", false, "overwrite an existing --out file")
	}
}

// resolve returns the provider row and https host: --host builds a preset row, else the registry row's.
func resolve(ctx context.Context, f cfgFlags) (*providers.Provider, string, error) {
	reg, lerr := providers.Load() // nil reg: --host alone still renders, under the generic name
	name := resolveProfile(ctx, reg, f.profile)
	if name == "" {
		name = providers.GroupDatabricks
	}
	host, err := flagHost(f.host)
	if err != nil {
		return nil, "", err
	}
	if err := checkTokenHelperHost(reg, name, host, f.auth); err != nil {
		return nil, "", err
	}
	var row *providers.Provider
	if host == "" {
		if row, host, err = registryHost(reg, lerr, name); err != nil {
			return nil, "", err
		}
	}
	if row != nil {
		return row, host, nil
	}
	return synth(name, host)
}

// checkTokenHelperHost refuses a --host the harness token helper cannot serve: it resolves the named registry row.
func checkTokenHelperHost(reg *providers.Registry, name, host, auth string) error {
	if host == "" || auth != authHarness {
		return nil
	}
	if reg == nil || reg.Providers[name] == nil {
		return fmt.Errorf("--auth harness needs a registered profile, and %q is not one; use --auth cli", name)
	}
	if _, rowHost, err := registryHost(reg, nil, name); err == nil && rowHost != host {
		return fmt.Errorf("--host %s differs from profile %q host %s, whose token the helper mints; use --auth cli", host, name, rowHost)
	}
	return nil
}

func flagHost(h string) (string, error) {
	if h == "" {
		return "", nil
	}
	if n := oauth.NormalizeHost(h); n != "" {
		return n, nil
	}
	return "", fmt.Errorf("host %q is not a valid https workspace URL", h)
}

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

// synth expands a bare databricks preset row for host through the registry's own loader.
func synth(name, host string) (*providers.Provider, string, error) {
	doc, err := json.Marshal(map[string]any{"providers": map[string]any{providers.GroupDatabricks: map[string]any{name: map[string]any{
		"preset": providers.PresetDatabricks, "host": host}}}})
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

func (f cfgFlags) options(ctx context.Context, target, self string) (renderOptions, error) {
	p, host, err := resolve(ctx, f)
	if err != nil {
		return renderOptions{}, err
	}
	return renderOptions{Target: target, Host: host, P: p, Auth: f.auth, Self: self,
		Model: f.model, Set: f.set, Unset: f.unset}, nil
}

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
	o, err := f.options(context.Background(), target, self)
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

func findClaude() (string, error) {
	if p, err := paths.AgentBin("claude"); err == nil {
		return p, nil
	}
	return exec.LookPath("claude")
}

// rescueDeny is what rescue drops beyond proc.ScrubDeny, so only the scratch settings steer claude.
var rescueDeny = []string{"HARNESS_*", "ANTHROPIC_*", "CLAUDE_CODE_USE_*", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}

func scrubbed(environ []string) []string {
	return proc.Drop(environ, nil, slices.Concat(proc.ScrubDeny, rescueDeny))
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
	res, file, err := renderScratch(context.Background(), fs, f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: %v\n", err)
		return 2
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

func renderScratch(ctx context.Context, fs *flag.FlagSet, f cfgFlags) (renderResult, string, error) {
	if err := checkProfileFlag(fs, f.profile); err != nil {
		return renderResult{}, "", err
	}
	self, _ := os.Executable()
	o, err := f.options(ctx, targetClaude, self)
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
