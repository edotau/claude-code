package databricks

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/dbxconfig"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// cfgFlags are the flags `config` and `rescue` share.
type cfgFlags struct {
	env, provider, host, wire, auth, out, model string
	force                                       bool
	set, unset                                  multi
}

var configPage = helptext.Page{
	Usage: "claude-code databricks config <claude|codex> [flags]",
	About: "Render a ready-to-use Databricks config from embedded templates. " +
		"Prints to stdout (never a token); --out writes it 0600.",
}

var rescuePage = helptext.Page{
	Usage: "claude-code databricks rescue [flags] [-- claude args...]",
	About: "Break-glass: run claude with a scratch settings file, no router " +
		"and no harness overlay. The file is deleted when claude exits. " +
		"--print shows the file and command only and leaves the file.",
}

func bind(fs *flag.FlagSet, f *cfgFlags, authDef string, withOut bool) {
	fs.StringVar(&f.env, "env", "", "workspace: dev, qa or prod (default: HARNESS_PROVIDER's, else qa)")
	fs.StringVar(&f.provider, "provider", "", "registry provider to take the host from")
	fs.StringVar(&f.host, "host", "", "https://host override, skips the vault")
	fs.StringVar(&f.wire, "wire", "", "gateway or serving (default: the row's, else serving)")
	fs.StringVar(&f.model, "model", "", "model override, canonical or wire id")
	fs.StringVar(&f.auth, "auth", authDef, "harness (token helper) or cli (databricks CLI)")
	fs.Var(&f.set, "set", "K=V env key to add or change (repeatable)")
	fs.Var(&f.unset, "unset", "K env key to remove (repeatable)")
	if withOut {
		fs.StringVar(&f.out, "out", "", "write to FILE (0600) instead of stdout")
		fs.BoolVar(&f.force, "force", false, "overwrite an existing --out file")
	}
}

// resolve returns the expanded provider row and its https host: --host builds a preset row in memory,
// else the registry row (re-expanded from its host when --wire differs from the row's).
func resolve(f cfgFlags) (*providers.Provider, string, error) {
	name := f.provider
	if name == "" {
		env := envOrDefault(f.env)
		if !slices.Contains(providers.DatabricksEnvs, env) {
			return nil, "", fmt.Errorf("--env must be dev, qa or prod (got %q)", env)
		}
		name = "databricks-" + env
	} else if f.env != "" {
		return nil, "", errors.New("use --env or --provider, not both")
	}
	if f.wire != "" && f.wire != providers.WireServing && f.wire != providers.WireGateway {
		return nil, "", fmt.Errorf("--wire must be gateway or serving (got %q)", f.wire)
	}
	host := strings.TrimRight(f.host, "/")
	var row *providers.Provider
	if host == "" {
		reg, lerr := providers.Load()
		if reg == nil {
			return nil, "", fmt.Errorf("cannot read the registry (%v); pass --host URL", lerr)
		}
		if row = reg.Providers[name]; row == nil {
			return nil, "", fmt.Errorf("no provider %q; pass --host URL", name)
		}
		for _, raw := range []string{row.Host, row.BaseURL} {
			if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && u.Host != "" {
				host = "https://" + u.Host
				break
			}
		}
		if host == "" {
			return nil, "", fmt.Errorf("provider %q has no host; pass --host URL", name)
		}
	}
	if row != nil && (f.wire == "" || f.wire == row.Options.Wire || (f.wire == providers.WireServing && row.Options.Wire == "")) {
		return row, host, nil
	}
	return synth(name, host, f.wire)
}

// synth expands a bare databricks preset row for host and wire through the registry's own loader.
func synth(name, host, wire string) (*providers.Provider, string, error) {
	doc, err := json.Marshal(map[string]any{"providers": map[string]any{name: map[string]any{
		"preset": providers.PresetDatabricks, "host": host, "options": map[string]string{"wire": wire}}}})
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

func (f cfgFlags) options(target, self string) (dbxconfig.Options, error) {
	p, host, err := resolve(f)
	if err != nil {
		return dbxconfig.Options{}, err
	}
	return dbxconfig.Options{Target: target, Host: host, P: p, Auth: f.auth, Self: self,
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
	bind(fs, &f, dbxconfig.AuthHarness, true)
	if code, done := helptext.Parse(fs, args[1:]); done {
		return code
	}
	self, _ := os.Executable()
	o, err := f.options(target, self)
	if err == nil {
		var res dbxconfig.Result
		if res, err = dbxconfig.Render(o); err == nil {
			return emit(f, target, res)
		}
	}
	fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
	return 2
}

func emit(f cfgFlags, target string, res dbxconfig.Result) int {
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, "note: "+res.Note)
	}
	if f.out == "" {
		_, _ = io.WriteString(os.Stdout, res.Text)
		return 0
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if f.force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	w, err := os.OpenFile(f.out, flags, 0o600)
	if err == nil {
		_, err = io.WriteString(w, res.Text)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if errors.Is(err, os.ErrExist) {
		err = fmt.Errorf("%s exists; pass --force to overwrite", f.out)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "wrote %s config to %s\n", target, f.out)
	return 0
}

func configUsage() {
	fs := helptext.NewFlagSet("claude-code databricks config", configPage)
	bind(fs, &cfgFlags{}, dbxconfig.AuthHarness, true)
	fs.Usage()
}

// findClaude locates the claude binary: the harness vendor link, else PATH.
func findClaude() (string, error) {
	if p, err := paths.VendorBin("claude"); err == nil {
		return p, nil
	}
	return exec.LookPath("claude")
}

// scrubbed drops every inherited HARNESS_* and ANTHROPIC_* var so claude sees only the scratch settings.
func scrubbed(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if strings.HasPrefix(kv, "HARNESS_") || strings.HasPrefix(kv, "ANTHROPIC_") {
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
	bind(fs, &f, dbxconfig.AuthCLI, false)
	fs.BoolVar(&print, "print", false, "print the file and command, do not run")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	self, _ := os.Executable()
	o, err := f.options(dbxconfig.TargetClaude, self)
	var res dbxconfig.Result
	if err == nil {
		res, err = dbxconfig.Render(o)
	}
	var file string
	if err == nil {
		file, err = writeScratch(res.Text)
	}
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

// writeScratch writes text to a fresh 0600 file under the OS temp dir.
func writeScratch(text string) (string, error) {
	w, err := os.CreateTemp("", "dbx-rescue-*.json")
	if err != nil {
		return "", err
	}
	defer w.Close()
	if _, err := w.WriteString(text); err != nil {
		return "", err
	}
	return filepath.Clean(w.Name()), os.Chmod(w.Name(), 0o600)
}

func rescueUsage() {
	fs := helptext.NewFlagSet("claude-code databricks rescue", rescuePage)
	bind(fs, &cfgFlags{}, dbxconfig.AuthCLI, false)
	fs.Bool("print", false, "print the file and command, do not run")
	fs.Usage()
}
