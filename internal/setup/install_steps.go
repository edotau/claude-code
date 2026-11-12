package setup

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// install copies the binary into BinDir, links the agent CLIs, and seeds a missing settings.json
// (an existing one is never written). A real (non-dry-run) run prints one compact line per step, naming only
// non-ok items in full; --dry-run and --verbose keep the full step-by-step detail.
func install(w io.Writer, dryRun bool, verbose bool) error {
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
	installCompletion(w, dryRun)
	installClipboardProxy(w, dryRun)
	installClipBridge(w, target, dryRun)
	seedTmuxConf(w, dryRun)
	if err := linkUserBins(w, target, dryRun); err != nil {
		return err
	}
	settingsErr := installSettings(w, dryRun)
	if werr := installWindowsSettings(w, dryRun); werr != nil {
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
// compact one-line-per-step form of install; the extras line prints last. Warnings always print in full.
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
	r, err := configure.SnapshotConfig("pre-install")
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
			data, err := os.ReadFile(self)
			if err == nil {
				err = paths.AtomicWrite(target, data, 0o755)
			}
			if err != nil {
				return "", err
			}
		}
	}
	return target, nil
}

// agentsCompactLine prints one "tools" line (kept) and one "links" line (linked); any other status prints in full.
func agentsCompactLine(w io.Writer) {
	var linked, kept []string
	for _, r := range tools.LinkResults(nil, false) {
		switch {
		case r.Status == "ok" || r.Status == "link":
			linked = append(linked, r.Bin)
		case r.Status == "keep" && strings.Contains(r.Detail, "installed here"):
			kept = append(kept, r.Bin)
		default:
			fmt.Fprintf(w, "  %-8s %s\n", r.Status, r.Detail)
		}
	}
	if len(kept) > 0 {
		fmt.Fprintf(w, "tools    kept: %s\n", strings.Join(kept, " "))
	}
	if len(linked) > 0 {
		fmt.Fprintf(w, "links    %s linked\n", strings.Join(linked, " "))
	}
}

// linkOnPathCompact runs linkUserBins, suppressing its routine "link" success line; a "warning" (not on
// PATH) or "skip" (a real file is there) line always prints in full.
func linkOnPathCompact(w io.Writer, target string) error {
	var buf bytes.Buffer
	if err := linkUserBins(&buf, target, false); err != nil {
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

// windowsSettingsCompact runs installWindowsSettings; its lines are already one per state, plus any error.
func windowsSettingsCompact(w io.Writer) {
	if err := installWindowsSettings(w, false); err != nil {
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
	for _, a := range agent.DefaultAgents {
		var buf bytes.Buffer
		o := agentconfig.RenderOptions{Agents: []string{a}, Check: true, Out: &buf}
		drift, err := agent.RenderConfigs(o)
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
		if _, err := agent.RenderConfigs(o); err != nil {
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
	if _, err := helptext.Install("claude-code"); err != nil {
		fmt.Fprintf(w, "completion skip  %v\n", err)
	} else {
		tokens = append(tokens, "completion ok")
	}
	tokens = append(tokens, clipboardExtras(w)...)
	var tbuf bytes.Buffer
	if tok := seedTmuxConf(&tbuf, false); tok != "" {
		tokens = append(tokens, "tmux "+tok)
	} else {
		fmt.Fprint(w, tbuf.String()) // the skip line, in full
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
		if path, err := helptext.Install("claude-code"); err != nil {
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

// installGitHooks points core.hooksPath at the repo's .githooks (its pre-push body is commented out); best-effort.
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

// linkUserBins links claude-code, and the pinned rtk when installed, into userBinDir so PATH finds them.
func linkUserBins(w io.Writer, target string, dryRun bool) error {
	if err := linkOnPath(w, "claude-code", target, dryRun); err != nil {
		return err
	}
	if rtk := paths.RTKPinned(); paths.ExecutableRealPath(rtk) != "" {
		return linkOnPath(w, "rtk", rtk, dryRun)
	}
	return nil
}

// linkOnPath points userBinDir/name at target; a real file there is left alone.
func linkOnPath(w io.Writer, name, target string, dryRun bool) error {
	dir := userBinDir()
	if dir == "" {
		return errors.New("link " + name + ": cannot resolve the home directory")
	}
	link := filepath.Join(dir, name)
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

// sameFile is true when target already is self, or holds self's bytes and is executable.
func sameFile(self, target string) bool {
	if real, _ := filepath.EvalSymlinks(target); real == self {
		return true
	}
	fi, err := os.Stat(target)
	if err != nil || fi.Mode()&0o111 == 0 {
		return false
	}
	if si, err := os.Stat(self); err != nil || si.Size() != fi.Size() {
		return false
	}
	return sameBytes(self, target)
}

// sameBytes streams both files in chunks and is true only when every byte matches and both read cleanly.
func sameBytes(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if ea != nil || eb != nil {
			ok := func(e error) bool { return errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) }
			return ok(ea) && ok(eb)
		}
	}
}

//go:embed tmux.conf
var tmuxConf []byte

// tmuxConfPaths are the files tmux reads at startup; the first is where a missing config is seeded.
var tmuxConfPaths = func() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return []string{filepath.Join(home, ".tmux.conf"), filepath.Join(xdg, "tmux", "tmux.conf")}
}

// seedTmuxConf writes the harness tmux.conf only when no tmux config exists; returns "kept", "wrote" or "" (skipped).
func seedTmuxConf(w io.Writer, dryRun bool) string {
	paths := tmuxConfPaths()
	if len(paths) == 0 {
		fmt.Fprintln(w, "tmux     skip  home directory unresolved")
		return ""
	}
	for _, p := range paths {
		_, err := os.Stat(p)
		if err == nil {
			fmt.Fprintf(w, "tmux     %s exists (kept)\n", p)
			return "kept"
		}
		if !errors.Is(err, fs.ErrNotExist) { // unknown state: never seed over a config we could not probe
			fmt.Fprintf(w, "tmux     skip  %v\n", err)
			return ""
		}
	}
	if dryRun {
		fmt.Fprintf(w, "tmux     would write %s (skipped: --dry-run)\n", paths[0])
		return ""
	}
	f, err := os.OpenFile(paths[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // O_EXCL: never truncate a racing writer's config
	if errors.Is(err, fs.ErrExist) {
		fmt.Fprintf(w, "tmux     %s exists (kept)\n", paths[0])
		return "kept"
	}
	if err == nil {
		_, err = f.Write(tmuxConf)
		err = errors.Join(err, f.Close())
		if err != nil {
			err = errors.Join(err, os.Remove(paths[0])) // a truncated conf must not pass for an existing one
		}
	}
	if err != nil {
		fmt.Fprintf(w, "tmux     skip  %v\n", err)
		return ""
	}
	fmt.Fprintf(w, "tmux     wrote %s\n", paths[0])
	return "wrote"
}
