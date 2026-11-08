package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/settings"
)

// Install copies the binary into BinDir, links the agent CLIs, and seeds a missing settings.json
// (an existing one is never written). A real (non-dry-run) run prints one compact line per step, naming only
// non-ok items in full; --dry-run and --verbose keep the full step-by-step detail.
func Install(w io.Writer, dryRun bool, verbose bool) error {
	if dryRun || verbose {
		return installVerbose(w, dryRun)
	}
	return installCompact(w)
}

// installVerbose is the original, full-detail install: one or more lines per step, every status named.
func installVerbose(w io.Writer, dryRun bool) error {
	snapshotPreInstall(w, dryRun, false)
	target, err := installBinaryAndLinks(w, dryRun)
	if err != nil {
		return err
	}
	installFormats(w, dryRun)
	installCompletion(w, dryRun)
	installClipboardProxy(w, dryRun)
	installClipBridge(w, target, dryRun)
	seedTmuxConf(w, dryRun)
	if err := linkOnPath(w, target, dryRun); err != nil {
		return err
	}
	settingsErr := installSettings(w, dryRun)
	if werr := InstallWindowsSettings(w, dryRun); werr != nil {
		fmt.Fprintf(w, "settings (windows) skip  %v\n", werr)
	}
	syncAgents(w, dryRun)
	if !dryRun {
		fmt.Fprintf(w, "agents   %s\n", agentConfigsSummary(w))
	}
	installGitHooks(w, dryRun)
	if !dryRun {
		preflightStep(w, target)
	}
	if settingsErr != nil {
		fmt.Fprintf(w, "settings skip  %v (agent sync and installGitHooks still ran)\n", settingsErr)
	}
	return settingsErr
}

// installCompact runs every install step for real (never dry-run) and prints one line per step:
// install (binary + snapshot), agents, settings, settings (windows) when applicable, agents, extras.
// Steps run in installVerbose's order; the extras line prints last. Warnings always print in full.
func installCompact(w io.Writer) error {
	commit := snapshotPreInstall(w, false, true)
	target, err := installBinaryTarget(io.Discard, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, installLine(target, commit))
	agentsCompactLine(w)
	extras := extrasCompact(w)
	defer func() {
		if extras != "" {
			fmt.Fprintln(w, extras)
		}
	}()
	if err := linkOnPathCompact(w, target); err != nil {
		return err
	}
	settingsLine, settingsErr := settingsStatus(false)
	if settingsLine != "" {
		fmt.Fprintln(w, settingsLine)
	}
	windowsSettingsCompact(w)
	agentsSyncCompact(w)
	installGitHooks(w, false)
	preflightStep(w, target)
	if settingsErr != nil {
		fmt.Fprintf(w, "settings skip  %v (agent sync and installGitHooks still ran)\n", settingsErr)
	}
	return settingsErr
}

// preflightStep refreshes stale workspace listings and prints one live pong row per pinned agent; never fails install.
func preflightStep(w io.Writer, bin string) {
	reg, err := providers.Load()
	if err != nil {
		return // no registry: nothing to check
	}
	preflight.RunInstallStep(context.Background(), w, reg, bin, time.Now())
}

// installLine renders the "install" summary line: the installed binary path and, when the pre-install
// history snapshot produced a commit, its short hash.
func installLine(target, commit string) string {
	line := "install  " + paths.ShortHome(target)
	if commit != "" {
		line += fmt.Sprintf(" (%s snapshot)", commit)
	}
	return line
}

// snapshotPreInstall records history before overwrite, returning the commit hash; quiet drops the success line.
func snapshotPreInstall(w io.Writer, dryRun, quiet bool) string {
	if dryRun {
		return ""
	}
	r, err := history.SnapshotConfig("pre-install")
	if err != nil {
		fmt.Fprintf(w, "warning  history snapshot failed: %v · continuing; the binary has no rollback point\n", err)
		return ""
	}
	if !quiet && r.Commit != "" {
		fmt.Fprintf(w, "history  %s (pre-install snapshot)\n", r.Commit)
	}
	return r.Commit
}

// installBinaryAndLinks copies claude-code into BinDir and links each agent CLI there, returning the target path.
func installBinaryAndLinks(w io.Writer, dryRun bool) (string, error) {
	target, err := installBinaryTarget(w, dryRun)
	if err != nil {
		return "", err
	}
	tools.Link(w, nil, dryRun)
	return target, nil
}

// installBinaryTarget copies claude-code into BinDir (printing a "copy" line to w) without linking agents;
// installCompact passes io.Discard and links agents separately via agentsCompactLine.
func installBinaryTarget(w io.Writer, dryRun bool) (string, error) {
	self, err := selfBinary()
	if err != nil {
		return "", err
	}
	target := filepath.Join(paths.BinDir(), "claude-code")
	if !sameFile(self, target) {
		fmt.Fprintf(w, "copy     %s → %s\n", self, target)
		if !dryRun {
			if err := copyBinary(self, target); err != nil {
				return "", err
			}
		}
	}
	return target, nil
}

// agentsCompactLine groups tools.LinkResults into one "links" line (linked / kept), printing any
// other status (skip, remove, missing, error) in full on its own line.
func agentsCompactLine(w io.Writer) {
	var linked, kept []string
	for _, r := range tools.LinkResults(nil, false) {
		switch {
		case r.Status == "ok" || r.Status == "link":
			linked = append(linked, r.Bin)
		case r.Status == "keep" && strings.Contains(r.Detail, "installed here") && !strings.Contains(r.Detail, "also on PATH"):
			kept = append(kept, r.Bin)
		default:
			fmt.Fprintf(w, "  %-8s %s\n", r.Status, r.Detail)
		}
	}
	var parts []string
	if len(linked) > 0 {
		parts = append(parts, strings.Join(linked, " ")+" linked")
	}
	if len(kept) > 0 {
		parts = append(parts, strings.Join(kept, " ")+" kept (installed here)")
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "links    %s\n", strings.Join(parts, " · "))
	}
}

// linkOnPathCompact runs linkOnPath, suppressing its routine "link" success line; a "warning" (not on
// PATH) or "skip" (a real file is there) line always prints in full.
func linkOnPathCompact(w io.Writer, target string) error {
	var buf bytes.Buffer
	if err := linkOnPath(&buf, target, false); err != nil {
		return err
	}
	for _, l := range splitNonEmpty(buf.String()) {
		if !strings.HasPrefix(l, "link ") {
			fmt.Fprintln(w, l)
		}
	}
	return nil
}

// installSettings seeds a missing settings.json; an existing one is never written, only checked for validity.
func installSettings(w io.Writer, dryRun bool) error {
	line, err := settingsStatus(dryRun)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, line)
	return nil
}

// settingsStatus seeds settings.json when it is missing (unless dryRun) and returns the one status line.
func settingsStatus(dryRun bool) (string, error) {
	file := settings.SettingsFile()
	short := paths.ShortHome(file)
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) && dryRun {
		return "settings " + short + " missing · would seed from the template; --dry-run wrote nothing", nil
	}
	seeded, err := settings.SeedSettings(file, nil)
	if err != nil {
		return "", err
	}
	if seeded {
		return "settings " + short + " seeded from the template", nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if err := settings.Check(data); err != nil {
		return fmt.Sprintf("settings %s kept (yours) · invalid: %v", short, err), nil
	}
	return "settings " + short + " kept (yours) · valid", nil
}

// windowsSettingsCompact runs InstallWindowsSettings; its lines are already one per state, plus any error.
func windowsSettingsCompact(w io.Writer) {
	if err := InstallWindowsSettings(w, false); err != nil {
		fmt.Fprintf(w, "settings (windows) error  %v\n", err)
	}
}

// agentsSyncCompact runs the agents/skills/instructions projection and reports the agents synced, or
// the sync's own failure note in full.
func agentsSyncCompact(w io.Writer) {
	if rc := project.Sync(project.Options{Out: io.Discard}); rc != 0 {
		fmt.Fprintf(w, "agents   sync exit %d (see `claude-code agents sync`) · %s\n", rc, agentConfigsSummary(w))
		return
	}
	fmt.Fprintf(w, "agents   synced: %s · %s\n", strings.Join(project.Names(), " "), agentConfigsSummary(w))
}

// agentConfigsSummary re-renders the agent configs `configure --restore` writes (codex, opencode, gemini, copilot) and
// returns a one-token summary; best-effort, so a render failure warns on w and never fails install.
func agentConfigsSummary(w io.Writer) string {
	var changed []string
	for _, a := range client.DefaultAgents {
		var buf bytes.Buffer
		o := render.RenderOptions{Agents: []string{a}, Check: true, Out: &buf}
		drift, err := client.RenderConfigs(o)
		if err != nil {
			fmt.Fprintf(w, "agents   config render skip (%s)  %v\n", a, err)
			continue
		}
		if strings.Contains(buf.String(), "skipping agent configs") {
			return "configs skipped"
		}
		if !drift {
			continue
		}
		o.Check = false
		if _, err := client.RenderConfigs(o); err != nil {
			fmt.Fprintf(w, "agents   config render skip (%s)  %v\n", a, err)
			continue
		}
		changed = append(changed, a)
	}
	if len(changed) == 0 {
		return "configs unchanged"
	}
	return "configs rendered: " + strings.Join(changed, " ")
}

// extrasCompact runs the completion, clipboard-proxy and tmux-seed steps and returns their one-line summary;
// skip notes print in full immediately.
func extrasCompact(w io.Writer) string {
	var tokens []string
	if tok := formatsCompact(w); tok != "" {
		tokens = append(tokens, tok)
	}
	if _, err := completion.Install("claude-code"); err != nil {
		fmt.Fprintf(w, "completion skip  %v\n", err)
	} else {
		tokens = append(tokens, "completion ok")
	}
	tokens = append(tokens, clipboardExtras(w)...)
	var tbuf bytes.Buffer
	seedTmuxConf(&tbuf, false)
	switch txt := tbuf.String(); {
	case strings.Contains(txt, "exists (kept)"):
		tokens = append(tokens, "tmux kept")
	case strings.Contains(txt, "wrote"):
		tokens = append(tokens, "tmux wrote")
	case strings.Contains(txt, "skip"):
		fmt.Fprint(w, txt)
	}
	if len(tokens) == 0 {
		return ""
	}
	return "extras   " + strings.Join(tokens, " · ")
}

func clipboardExtras(w io.Writer) []string {
	var tokens []string
	var cbuf bytes.Buffer
	if err := clipboard.InstallProxy(&cbuf, false); err != nil {
		fmt.Fprintf(w, "clipboard skip  %v\n", err)
	} else if cbuf.Len() > 0 {
		if fields := strings.Fields(cbuf.String()); len(fields) > 0 && fields[0] == "skip" {
			fmt.Fprintf(w, "clipboard %s\n", strings.TrimSpace(cbuf.String()))
		} else {
			tokens = append(tokens, "clipboard ok")
		}
	}
	var lbuf bytes.Buffer
	if err := installClipAgent(&lbuf, filepath.Join(paths.BinDir(), "claude-code"), false); err != nil {
		fmt.Fprintf(w, "clip agent skip  %v\n", err)
	} else if lbuf.Len() > 0 {
		tokens = append(tokens, "clip agent ok")
	}
	return tokens
}

// splitNonEmpty splits s on newlines, dropping the trailing blank line and any other empty lines.
func splitNonEmpty(s string) []string {
	var out []string
	for l := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// installCompletion is best-effort — an unwritable completions dir (no bash-completion package,
// read-only $HOME) must never fail the install that got the binary in place.
func installCompletion(w io.Writer, dryRun bool) {
	switch {
	case dryRun:
		fmt.Fprintln(w, "completion would install bash tab-completion (skipped: --dry-run)")
	default:
		if path, err := completion.Install("claude-code"); err != nil {
			fmt.Fprintf(w, "completion skip  %v\n", err)
		} else {
			fmt.Fprintf(w, "completion %s\n", path)
		}
	}
}

// installClipboardProxy ensures the WSL wl-paste proxy; a no-op off WSL, and best-effort like
// completion — a write failure must never fail the install that got the binary in place.
func installClipboardProxy(w io.Writer, dryRun bool) {
	if err := clipboard.InstallProxy(w, dryRun); err != nil {
		fmt.Fprintf(w, "  %-17s %v\n", "skip", "wl-paste proxy: "+err.Error())
	}
}

// installClipAgent is a var so tests never bootstrap (or boot out) the developer's real launchd agent.
var installClipAgent = clipboard.InstallLaunchAgent

// installClipBridge installs the macOS clipboard-bridge launchd agent; best-effort, a no-op off macOS.
func installClipBridge(w io.Writer, bin string, dryRun bool) {
	if err := installClipAgent(w, bin, dryRun); err != nil {
		fmt.Fprintf(w, "  %-17s %v\n", "skip", "clip agent: "+err.Error())
	}
}

// installGitHooks points core.hooksPath at the repo's .githooks (its pre-push is a no-op by choice); best-effort.
func installGitHooks(w io.Writer, dryRun bool) {
	root := paths.ConfigDir()
	if _, err := os.Stat(filepath.Join(root, ".githooks")); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cur, _ := exec.CommandContext(ctx, "git", "-C", root, "config", "--get", "core.hooksPath").Output()
	if strings.TrimSpace(string(cur)) == ".githooks" {
		return
	}
	fmt.Fprintln(w, "githooks core.hooksPath → .githooks")
	if dryRun {
		return
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "config", "core.hooksPath", ".githooks").CombinedOutput(); err != nil {
		fmt.Fprintf(w, "  githooks skip  %v %s\n", err, strings.TrimSpace(string(out)))
	}
}

// syncAgents projects agents/, skills/ and the agent instruction files into every agent home so they track the
// repo; best-effort, like completion.
func syncAgents(w io.Writer, dryRun bool) {
	fmt.Fprintln(w, "agents   sync agents/ + skills/ + instructions into agent homes (claude-code agents sync)")
	if rc := project.Sync(project.Options{DryRun: dryRun, Out: io.Discard}); rc != 0 {
		fmt.Fprintf(w, "  agents sync exit %d — run `claude-code agents sync` for details\n", rc)
	}
}

// linkOnPath points userBinDir/claude-code at the installed binary; a real file there is left alone.
func linkOnPath(w io.Writer, target string, dryRun bool) error {
	dir := userBinDir()
	link := filepath.Join(dir, "claude-code")
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		fmt.Fprintf(w, "skip     %s (a real file is there)\n", link)
		return nil
	}
	fmt.Fprintf(w, "link     %s → %s\n", link, target)
	if !strings.Contains(string(os.PathListSeparator)+os.Getenv("PATH")+string(os.PathListSeparator),
		string(os.PathListSeparator)+dir+string(os.PathListSeparator)) {
		fmt.Fprintf(w, "warning  %s is not on PATH; add it in your shell profile\n", dir)
	}
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return paths.SwapSymlink(target, link) // the link is never missing, racers don't collide
}

// sameFile is true when target already is, or holds the same bytes as, self.
func sameFile(self, target string) bool {
	if real, _ := filepath.EvalSymlinks(target); real == self {
		return true
	}
	a, err1 := os.ReadFile(self)
	b, err2 := os.ReadFile(target)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

func copyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(dst, data, 0o755)
}

// installFormats runs schemas.MigrateAll and prints one line per migrated format, or "current".
func installFormats(w io.Writer, dryRun bool) {
	done, err := schemas.MigrateAll(dryRun)
	for _, line := range formatsLines(done, dryRun) {
		fmt.Fprintln(w, line)
	}
	if len(done) == 0 && err == nil {
		fmt.Fprintln(w, "formats  current")
	}
	if err != nil {
		fmt.Fprintf(w, "formats skip  %v\n", err)
	}
}

func formatsLines(done []schemas.Stale, dryRun bool) []string {
	verb := "migrated"
	if dryRun {
		verb = "would migrate"
	}
	type key struct {
		f        string
		from, to int
	}
	counts := map[key]int{}
	var order []key
	for _, s := range done {
		k := key{s.Format, s.From, s.To}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	var out []string
	for _, k := range order {
		n := counts[k]
		out = append(out, fmt.Sprintf("formats  %s %s v%d→v%d (%d %s)", verb, k.f, k.from, k.to, n, pluralFile(n)))
	}
	return out
}

func pluralFile(n int) string {
	if n == 1 {
		return "file"
	}
	return "files"
}

// formatsCompact migrates for real and returns an extras token ("" when nothing was stale); errors print in full.
func formatsCompact(w io.Writer) string {
	done, err := schemas.MigrateAll(false)
	if err != nil {
		fmt.Fprintf(w, "formats skip  %v\n", err)
	}
	if len(done) == 0 {
		return ""
	}
	return fmt.Sprintf("formats %d migrated", len(done))
}
