// Package tools installs and updates agent CLIs (into ~/.claude/bin or their existing dir) and reports the rest.
package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/network/browser"

	"github.com/edotau/claude-code/internal/paths"
)

// action installs or updates one tool; w gets progress lines, dryRun prints without changing anything.
type action func(ctx context.Context, w io.Writer, dir string, dryRun bool) error

// tool is one install-table row; nil Install = no fresh-install path, nil Update = Install is idempotent.
type tool struct {
	Name    string
	Binary  string // executable name, resolved via paths.LookPathReal (skips our own shims)
	Install action
	Update  action
	Hint    string // shown in status/doctor when Install and Update are both nil
	// UserLocal installs into ~/.local/bin, never the harness bin/.
	UserLocal bool
}

// tools is the install table in roster order; npmPackages must cover entries that also ship via npm.
var tools = []tool{
	{Name: agentid.Claude, Binary: agentid.Claude, Install: installClaude, UserLocal: true},
	{Name: agentid.Copilot, Binary: agentid.Copilot, Install: installCopilot},
	{Name: agentid.Codex, Binary: agentid.Codex, Install: installCodex},
	{Name: agentid.OpenCode, Binary: agentid.OpenCode, Install: installOpencode},
	{Name: "claude-science", Binary: "claude-science", Install: installClaudeScience, Update: updateClaudeScience},
	{Name: "agy", Binary: "agy", Install: installAgy, Update: updateAgy},
	{Name: "tailscale", Binary: "tailscale", Install: installTailscale},
	{Name: "browser", Install: installBrowser, Hint: "claude-code network browser --setup; CDP + chrome-devtools-mcp on WSL"},
	{Name: "clipboard", Install: installClipboard, Hint: "WSL only: wl-paste proxy for image paste"},
}

// Names lists every tool in roster order.
func Names() []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// lookup returns the tool by name.
func lookup(name string) (tool, bool) {
	if i := slices.IndexFunc(tools, func(t tool) bool { return t.Name == name }); i >= 0 {
		return tools[i], true
	}
	return tool{}, false
}

// Install installs/updates names (empty = all), retiring replaced npm-global copies and @google/gemini-cli;
// missingOnly installs only binaries absent from PATH and never updates a present one.
func Install(ctx context.Context, w io.Writer, names []string, dryRun, missingOnly bool) error {
	sel, err := resolve(names)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range sel {
		old := directInstall(t)
		if old == "" && t.Binary != "" {
			old, _ = paths.LookPathReal(t.Binary)
		}
		if missingOnly && (t.Binary == "" || old != "") {
			continue
		}
		fmt.Fprintf(w, "== %s ==\n", t.Name)
		dir := installDir(t, old)
		if err := runOne(ctx, w, t, old, dir, dryRun); err != nil {
			fmt.Fprintf(w, "  error: %v\n", err)
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		npmCleanupFor(ctx, w, t.Name, landedDir(t, dir), dryRun)
	}
	cleanupGeminiCLI(ctx, w, dryRun)
	Link(w, names, dryRun)
	return errors.Join(errs...)
}

// landedDir is where runOne wrote t: codex always lands in BinDir (installCodex redirects).
func landedDir(t tool, dir string) string {
	if t.Name == agentid.Codex {
		return paths.BinDir()
	}
	return dir
}

func resolve(names []string) ([]tool, error) {
	if len(names) == 0 {
		return tools, nil
	}
	sel := make([]tool, 0, len(names))
	for _, n := range names {
		t, ok := lookup(n)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q (%s)", n, strings.Join(Names(), ", "))
		}
		sel = append(sel, t)
	}
	return sel, nil
}

// runOne updates a present tool that has its own Update, else (re)runs the idempotent Install; else report-only.
func runOne(ctx context.Context, w io.Writer, t tool, oldPath, dir string, dryRun bool) error {
	update := oldPath != "" && t.Update != nil
	if (update || t.Install != nil) && t.Binary != "" { // binary-less tools (browser, clipboard) ignore dir
		fmt.Fprintf(w, "  %-8s %s\n", "into", dir)
	}
	switch {
	case update:
		return t.Update(ctx, w, dir, dryRun)
	case t.Install != nil:
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

// installDir: a direct bin/ install, else the existing install's dir, else bin/ for an agent, else ~/.local/bin.
func installDir(t tool, oldPath string) string {
	if t.UserLocal {
		return localBinDir()
	}
	_, linked := paths.AgentName(t.Name)
	switch {
	case directInstall(t) != "":
		return paths.BinDir()
	case oldPath != "":
		return filepath.Dir(oldPath)
	case linked:
		return paths.BinDir()
	}
	return localBinDir()
}

// directInstall is BinDir/<bin> when an agent's CLI is a regular file there (a direct install), else "".
func directInstall(t tool) string {
	bin, ok := paths.AgentName(t.Name)
	if !ok {
		return ""
	}
	p := filepath.Join(paths.BinDir(), bin)
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
		return p
	}
	return ""
}

// localBinDir is the fallback install target for non-agent tools; "" when the home dir is unknown.
func localBinDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "bin")
}

// installBinaryLocked atomically writes data as an executable at dir/name, under dir's bin lock.
func installBinaryLocked(w io.Writer, dir, name string, data []byte) error {
	return withBinLock(dir, func() error { return installBinary(w, dir, name, data) })
}

// withBinLock runs fn holding dir's bin lock, so link.go's check-then-act cannot race it.
func withBinLock(dir string, fn func() error) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("install dir %q is not absolute (HOME unset?)", dir)
	}
	return paths.WithFileLock(binLockPath(dir), fn)
}

// installBinary atomically writes data as an executable at dir/name; the caller must hold dir's bin lock.
func installBinary(w io.Writer, dir, name string, data []byte) error {
	dest := filepath.Join(dir, name)
	fmt.Fprintf(w, "  install  %s (%d bytes)\n", dest, len(data))
	return paths.AtomicWrite(dest, data, 0o755)
}

// binLockPath is the lock file shared by every writer and linker of dir's agent binaries.
func binLockPath(dir string) string { return filepath.Join(dir, ".agent-bin") }

// npmPackages maps a Tool name to the npm package that also ships it, for post-install cleanup.
var npmPackages = map[string]string{agentid.Codex: "@openai/codex", agentid.OpenCode: "opencode-ai"}

// npmCleanupFor retires npm-global copies on PATH once dir/name answers --version.
func npmCleanupFor(ctx context.Context, w io.Writer, name, dir string, dryRun bool) {
	pkg, ok := npmPackages[name]
	if !ok {
		return
	}
	localPath := filepath.Join(dir, name)
	if !dryRun && FirstVersionLine(ctx, localPath, versionProbeTimeout) == "" {
		fmt.Fprintf(w, "  keep     npm-global %s (%s did not answer --version)\n", name, localPath)
		return
	}
	seen := make(map[string]bool)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		bin := filepath.Join(entry, name)
		prefix := cleanupPrefix(bin, localPath)
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		runNpmUninstall(ctx, w, dryRun, prefix, pkg)
	}
}

func cleanupPrefix(bin, localPath string) string {
	info, err := os.Stat(bin)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	if paths.SamePath(bin, localPath) {
		return ""
	}
	return npmPrefixFromBinary(bin)
}

// cleanupGeminiCLI uninstalls the deprecated @google/gemini-cli on every Install, whichever tools were named.
func cleanupGeminiCLI(ctx context.Context, w io.Writer, dryRun bool) {
	bin, err := paths.LookPathReal(agentid.Gemini)
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

// npmPrefixFromBinary returns bin's npm --prefix when it lives under lib/node_modules, else "".
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
	ctx, cancel := context.WithTimeout(ctx, installCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "npm", "uninstall", "-g", "--prefix", prefix, pkg).CombinedOutput()
	if err != nil {
		fmt.Fprintf(w, "    npm uninstall failed (best-effort): %v %s\n", err, strings.TrimSpace(string(out)))
	}
}

// installBrowser runs `network browser --setup` on WSL; elsewhere it only reports, never opening a page.
func installBrowser(_ context.Context, w io.Writer, dir string, dryRun bool) error {
	env := browser.NewEnv()
	env.DryRun = dryRun
	if !env.WSL {
		def, _ := browser.Default(env)
		fmt.Fprintf(w, "  %-17s %s\n", "report", cmp.Or(def.DisplayName, "no default browser")+
			" — run `claude-code network browser --setup` to connect Claude in Chrome")
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

// Status is one tool's resolved state: path and version are "" when it isn't found on PATH.
type Status struct {
	Name      string
	Path      string
	Version   string // first line of `<path> --version`, best-effort
	LocalBin  bool   // Path lives under ~/.local/bin
	NPMGlobal bool   // Path resolves through an npm-global lib/node_modules install
	Target    string // os.Readlink of BinDir/<bin> for an agent tool; "" when not a link
	Hint      string
}

// StatusAll reports every tool in roster order.
func StatusAll() []Status {
	var out []Status
	for _, t := range tools {
		if t.Binary != "" { // a setup-only row (browser) has no binary to report; doctor has its own browser row
			out = append(out, statusOne(t))
		}
	}
	return out
}

func statusOne(t tool) Status {
	s := Status{Name: t.Name, Hint: t.Hint}
	if bin, ok := paths.AgentName(t.Name); ok {
		s.Target = readlink(filepath.Join(paths.BinDir(), bin))
	}
	path, err := paths.LookPathReal(t.Binary)
	if err != nil {
		return s
	}
	s.Path = path
	local := localBinDir()
	s.LocalBin = local != "" && strings.HasPrefix(path, local+string(filepath.Separator))
	s.NPMGlobal = npmPrefixFromBinary(path) != ""
	s.Version = versionLine(path)
	return s
}
