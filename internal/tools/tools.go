// Package tools installs and updates vendor agent CLIs (into ~/.claude/bin or their existing dir) and reports the rest.
package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/clipboard"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// action installs or updates one tool; w gets progress lines, dryRun prints without changing anything.
type action func(ctx context.Context, w io.Writer, dir string, dryRun bool) error

// Tool is one row of the install table: Install is nil when there is no known fresh-install path
// (agy, tailscale); Update is nil when Install also serves as the update (rerunning it is idempotent).
type Tool struct {
	Name    string
	Binary  string // executable name, resolved via paths.LookPathReal (skips our own shims)
	Install action
	Update  action
	Hint    string // shown in status/doctor when Install and Update are both nil
}

// Tools is the install table in roster order; npmPackages below must cover any entry that also
// ships via npm, so a fresh install can retire the old global copy.
var Tools = []Tool{
	{Name: "claude", Binary: "claude", Install: installClaude},
	{Name: "copilot", Binary: "copilot", Install: installCopilot},
	{Name: "codex", Binary: "codex", Install: installCodex},
	{Name: "opencode", Binary: "opencode", Install: installOpencode},
	{Name: "claude-science", Binary: "claude-science", Install: installClaudeScience, Update: updateClaudeScience},
	{Name: "agy", Binary: "agy", Update: updateAgy, Hint: "no known install URL; claude-code configure signs it in"},
	{Name: "tailscale", Binary: "tailscale", Hint: "system install: make tailscale-cli"},
	{Name: "browser", Install: installBrowser, Hint: "claude-code web setup; CDP + chrome-devtools-mcp on WSL"},
	{Name: "clipboard", Install: installClipboard, Hint: "WSL only: wl-paste proxy for image paste"},
}

// Names lists every tool in roster order.
func Names() []string {
	out := make([]string, len(Tools))
	for i, t := range Tools {
		out[i] = t.Name
	}
	return out
}

// Lookup returns the tool by name.
func Lookup(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// InstallSelected installs/updates names (empty = all), retiring replaced npm-global copies and @google/gemini-cli.
func InstallSelected(ctx context.Context, w io.Writer, names []string, dryRun bool) error {
	sel, err := resolve(names)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range sel {
		fmt.Fprintf(w, "== %s ==\n", t.Name)
		old := directInstall(t)
		if old == "" {
			old, _ = paths.LookPathReal(t.Binary)
		}
		dir := installDir(t, old)
		if err := runOne(ctx, w, t, old, dir, dryRun); err != nil {
			fmt.Fprintf(w, "  error: %v\n", err)
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		npmCleanupFor(ctx, w, t.Name, dir, dryRun)
	}
	cleanupGeminiCLI(ctx, w, dryRun)
	return errors.Join(errs...)
}

func resolve(names []string) ([]Tool, error) {
	if len(names) == 0 {
		return Tools, nil
	}
	sel := make([]Tool, 0, len(names))
	for _, n := range names {
		t, ok := Lookup(n)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q (%s)", n, strings.Join(Names(), ", "))
		}
		sel = append(sel, t)
	}
	return sel, nil
}

// runOne updates a present tool that has its own Update, else (re)runs the idempotent Install; else report-only.
func runOne(ctx context.Context, w io.Writer, t Tool, oldPath, dir string, dryRun bool) error {
	switch {
	case oldPath != "" && t.Update != nil:
		fmt.Fprintf(w, "  %-8s %s\n", "into", dir)
		return t.Update(ctx, w, dir, dryRun)
	case t.Install != nil:
		fmt.Fprintf(w, "  %-8s %s\n", "into", dir)
		return t.Install(ctx, w, dir, dryRun)
	default:
		if oldPath != "" {
			fmt.Fprintf(w, "  present: %s\n", oldPath)
		}
		if t.Hint != "" {
			fmt.Fprintf(w, "  %s\n", t.Hint)
		}
		return nil
	}
}

// installDir: a direct bin/ install, else the existing install's dir, else bin/ for a vendor, else ~/.local/bin.
func installDir(t Tool, oldPath string) string {
	if t.Name == "claude" {
		return localBinDir()
	}
	_, vendor := paths.VendorName(t.Name)
	switch {
	case directInstall(t) != "":
		return paths.BinDir()
	case oldPath != "":
		return filepath.Dir(oldPath)
	case vendor:
		return paths.BinDir()
	}
	return localBinDir()
}

// directInstall is BinDir/<bin> when a vendor's CLI is a regular file there (a direct install), else "".
func directInstall(t Tool) string {
	bin, ok := paths.VendorName(t.Name)
	if !ok {
		return ""
	}
	p := filepath.Join(paths.BinDir(), bin)
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
		return p
	}
	return ""
}

// localBinDir is the fallback install target for non-vendor tools.
func localBinDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

// installBinary atomically writes data as an executable at dir/name.
func installBinary(w io.Writer, dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	fmt.Fprintf(w, "  install  %s (%d bytes)\n", dest, len(data))
	return paths.AtomicWrite(dest, data, 0o755)
}

// npmPackages maps a Tool name to the npm package that also ships it, for post-install cleanup.
var npmPackages = map[string]string{"codex": "@openai/codex", "opencode": "opencode-ai"}

// npmCleanupFor retires npm-global copies on PATH once dir/name answers --version.
func npmCleanupFor(ctx context.Context, w io.Writer, name, dir string, dryRun bool) {
	pkg, ok := npmPackages[name]
	if !ok {
		return
	}
	localPath := filepath.Join(dir, name)
	if !dryRun && firstVersionLine(localPath) == "" {
		fmt.Fprintf(w, "  keep     npm-global %s (%s did not answer --version)\n", name, localPath)
		return
	}
	seen := make(map[string]bool)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		bin := filepath.Join(dir, name)
		info, err := os.Stat(bin)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if sameExecutablePath(bin, localPath) {
			continue
		}
		prefix := npmPrefixFromBinary(bin)
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		runNpmUninstall(ctx, w, dryRun, prefix, pkg)
	}
}

func sameExecutablePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	realA, errA := filepath.EvalSymlinks(a)
	realB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && realA == realB
}

// cleanupGeminiCLI uninstalls the deprecated @google/gemini-cli whenever InstallSelected runs, independent
// of which tools were named: the binary is "gemini", so paths.LookPathReal also skips our own shim dir.
func cleanupGeminiCLI(ctx context.Context, w io.Writer, dryRun bool) {
	bin, err := paths.LookPathReal("gemini")
	if err != nil {
		return
	}
	prefix := npmPrefixFromBinary(bin)
	if prefix == "" {
		return
	}
	fmt.Fprintln(w, "== @google/gemini-cli (deprecated) ==")
	runNpmUninstall(ctx, w, dryRun, prefix, "@google/gemini-cli")
}

// npmPrefixFromBinary resolves bin's npm --prefix when it is an npm-global install (its real path
// runs through a lib/node_modules segment), or "" when it is not.
func npmPrefixFromBinary(bin string) string {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		real = bin
	}
	marker := string(filepath.Separator) + filepath.Join("lib", "node_modules") + string(filepath.Separator)
	i := strings.Index(real, marker)
	if i < 0 {
		return ""
	}
	return real[:i]
}

// runNpmUninstall is best-effort: an npm failure is reported, never fails the whole --tools run.
func runNpmUninstall(ctx context.Context, w io.Writer, dryRun bool, prefix, pkg string) {
	fmt.Fprintf(w, "  npm      uninstall -g --prefix %s %s\n", prefix, pkg)
	if dryRun {
		return
	}
	out, err := exec.CommandContext(ctx, "npm", "uninstall", "-g", "--prefix", prefix, pkg).CombinedOutput()
	if err != nil {
		fmt.Fprintf(w, "    npm uninstall failed (best-effort): %v %s\n", err, strings.TrimSpace(string(out)))
	}
}

// installBrowser: on WSL it runs `web setup` (start the Windows browser with CDP, register chrome-devtools-mcp);
// elsewhere it only reports — the Claude in Chrome page opens on an explicit `web setup`, never on install.
func installBrowser(_ context.Context, w io.Writer, dir string, dryRun bool) error {
	env := browser.NewEnv()
	env.DryRun = dryRun
	if !env.WSL {
		def, _ := browser.Default(env)
		fmt.Fprintf(w, "  %-17s %s\n", "report", cmp.Or(def.DisplayName, "no default browser")+
			" — run `claude-code web setup` to connect Claude in Chrome")
		return nil
	}
	acts, err := browser.Setup(env, "default")
	if err != nil { // no Windows browser or default: nothing to drive, not an install failure
		fmt.Fprintf(w, "  %-17s %v\n", "skip", err)
		return nil
	}
	var errs []error
	for _, a := range acts.Items {
		fmt.Fprintf(w, "  %-17s %s\n", a.Kind, a.Description)
		if a.Error != "" {
			errs = append(errs, fmt.Errorf("%s: %s", a.Kind, a.Error))
		}
	}
	return errors.Join(errs...)
}

// installClipboard writes the WSL wl-paste proxy (a no-op report off WSL).
func installClipboard(_ context.Context, w io.Writer, dir string, dryRun bool) error {
	return clipboard.InstallProxy(w, dryRun)
}
