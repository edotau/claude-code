package safety

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

func rtkOut(t *testing.T, in string) (cmd string, emitted bool) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // the test process cwd sits inside the real config tree
	res := RTKRewrite(core.ParseInput(strings.NewReader(in)))
	if res.UpdatedInput == nil {
		return "", false
	}
	s, _ := res.UpdatedInput["command"].(string)
	return s, true
}

// fakeRTK installs a stub rtk (HARNESS_RTK_BIN) and returns its absolute path. `rewrite <cmd>` prints
// FAKE_RTK_OUT, else "rtk <cmd>", and exits FAKE_RTK_EXIT (default 0).
func fakeRTK(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rtk")
	script := "#!/bin/sh\n[ \"$1\" = rewrite ] || exit 9\n" +
		"if [ -n \"$FAKE_RTK_OUT\" ]; then printf '%s' \"$FAKE_RTK_OUT\"; else printf '%s' \"rtk $2\"; fi\n" +
		"exit ${FAKE_RTK_EXIT:-0}\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shell.RTKBinaryEnv, bin)
	return bin
}

func rtkBashIn(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 120},
	})
	return string(b)
}

func TestRTKWrapsEligibleCommand(t *testing.T) {
	bin := fakeRTK(t)
	got, ok := rtkOut(t, rtkBashIn("git log --oneline -20"))
	if !ok || got != bin+" git log --oneline -20" {
		t.Fatalf("command = %q (emitted=%v), want the wrapped form", got, ok)
	}
}

func TestRTKSkipsCodex(t *testing.T) {
	bin := fakeRTK(t)
	payload := rtkBashIn("git log --oneline -20")
	t.Setenv("HARNESS_HOOK_AGENT", "codex")
	if vendor := core.ParseInput(strings.NewReader(payload)).Agent(); vendor != core.AgentCodex {
		t.Fatalf("Vendor() = %q, want %q", vendor, core.AgentCodex)
	}
	if got, ok := rtkOut(t, payload); ok {
		t.Fatalf("Codex command rewritten to %q", got)
	}
	t.Setenv("HARNESS_HOOK_AGENT", "")
	if got, ok := rtkOut(t, payload); !ok || got != bin+" git log --oneline -20" {
		t.Fatalf("Claude command = %q (emitted=%v), want the wrapped form", got, ok)
	}
}

func TestCodexSafetyScanStillBlocks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_HOOK_AGENT", "codex")
	fakeRTK(t)
	res := scanPayload(strings.NewReader(rtkBashIn("sudo apt install x")))
	if res.Decision != core.Block || !strings.Contains(res.Reason, "BLOCKED") {
		t.Fatalf("want a block naming BLOCKED: decision=%d reason=%q", res.Decision, res.Reason)
	}
}

func TestRTKMachineReadableFlagsMatchTokens(t *testing.T) {
	bin := fakeRTK(t)
	for _, cmd := range []string{
		"git checkout feature-zero",
		"git checkout feature--json",
		"git log --formatter=short",
		"kubectl get pods -o wide json",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); !ok || got != bin+" "+cmd {
			t.Errorf("%q: got %q (emitted=%v), want rewrite", cmd, got, ok)
		}
	}
	for _, flag := range rtkMachineReadable {
		variants := []string{flag, strings.ToUpper(flag)}
		if flag != "-o json" {
			variants = append(variants, flag+"=value")
		}
		for _, suffix := range variants {
			if got, ok := rtkOut(t, rtkBashIn("git log "+suffix)); ok {
				t.Errorf("real flag %q was wrapped to %q", suffix, got)
			}
		}
	}
	if got, ok := rtkOut(t, rtkBashIn(`kubectl get pods -o "json"`)); ok {
		t.Errorf("quoted JSON output was wrapped to %q", got)
	}
}

// updatedInput REPLACES the whole record, so every sibling key must survive the rewrite.
func TestRTKPreservesOtherInputKeys(t *testing.T) {
	fakeRTK(t)
	updated := RTKRewrite(core.ParseInput(strings.NewReader(rtkBashIn("npm install")))).UpdatedInput
	if updated["description"] != "d" {
		t.Errorf("description dropped: %#v", updated)
	}
	if updated["timeout"] == nil {
		t.Errorf("timeout dropped: %#v", updated)
	}
}

// The whole point of the guard: this harness's own hooks and workflows parse porcelain/JSON output.
// Compressing it would silently corrupt every parser downstream.
func TestRTKNeverWrapsMachineReadableOutput(t *testing.T) {
	fakeRTK(t)
	for _, cmd := range []string{
		"git status --porcelain",
		"git diff --name-only",
		"git diff --numstat -M HEAD",
		"git log --format=%H",
		"git ls-files -z",
		"npm ls --json",
		"cargo metadata --format-version 1",
		"kubectl get pods -o json",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — machine-readable output must bypass rtk", cmd, got)
		}
	}
}

// The flag list cannot catch these: `git rev-parse HEAD` carries no machine-readable flag, yet its
// one line is parsed verbatim by callers. Compressing a VALUE is the same corruption as compressing
// porcelain — the subcommand, not a flag, is what identifies it.
func TestRTKNeverWrapsValueProducingSubcommands(t *testing.T) {
	fakeRTK(t)
	for _, cmd := range []string{
		"git rev-parse HEAD",
		"git rev-parse --abbrev-ref HEAD",
		"git config --get user.email",
		"git describe --tags",
		"git ls-files",
		"git symbolic-ref refs/remotes/origin/HEAD",
		"git merge-base master HEAD",
		"git show-ref refs/heads/master",
		"git cat-file -p HEAD",
		"git branch --show-current",
		// Global options precede the subcommand. Classifying on fields[1] alone missed every one of
		// these, so their values reached the compressor — the exact corruption this guard exists to
		// prevent, and both forms appear in the harness's own scripts and hooks.
		"git -C /repo rev-parse HEAD",
		"git --no-pager config --get user.email",
		"git -c core.pager=cat rev-parse --abbrev-ref HEAD",
		"git --git-dir=/repo/.git rev-parse HEAD",
		"git -C /repo --no-pager ls-files",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — its output is a value to parse, not a report to read", cmd, got)
		}
	}
}

// An unrecognized global flag makes gitRTKSubcommand return "" (documented "fail closed"); rtkVetoed
// must treat that the same as a known value subcommand and skip wrapping, not fall through to wrap.
func TestRTKNeverWrapsUnrecognizedGlobalFlag(t *testing.T) {
	fakeRTK(t)
	for _, cmd := range []string{
		"git --unknown-global-flag rev-parse HEAD",
		"git -Z rev-parse HEAD",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — an unrecognized global flag must fail closed, not wrap", cmd, got)
		}
	}
}

// rtk rewrite owns chains: its rewritten text replaces the whole command, pipelines included.
func TestRTKRewritesChainsAndPipelines(t *testing.T) {
	bin := fakeRTK(t)
	for _, cmd := range []string{"git log --oneline -5 && ls", "git log -1; cat README.md", "git log --oneline | head -20"} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); !ok || got != bin+" "+cmd {
			t.Errorf("%q = %q (emitted=%v), want %q", cmd, got, ok, bin+" "+cmd)
		}
	}
}

func TestRTKSkipsAlreadyWrappedAndEmpty(t *testing.T) {
	bin := fakeRTK(t)
	for _, cmd := range []string{"rtk git log", bin + " git log", ""} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was rewritten to %q, want no rewrite", cmd, got)
		}
	}
}

// Anything but exit 0/3 with changed stdout leaves the command alone.
func TestRTKRewriteExitCodesAndFailures(t *testing.T) {
	fakeRTK(t)
	for _, tc := range []struct{ name, exit, out string }{
		{"exit 1", "1", "rtk ls"}, {"exit 2", "2", "rtk ls"}, {"exit 9", "9", "rtk ls"},
		{"unchanged", "0", "ls"}, {"empty", "0", " "},
	} {
		t.Setenv("FAKE_RTK_EXIT", tc.exit)
		t.Setenv("FAKE_RTK_OUT", tc.out)
		if got, ok := rtkOut(t, rtkBashIn("ls")); ok {
			t.Errorf("%s: rewritten to %q, want unchanged", tc.name, got)
		}
	}
	t.Setenv("FAKE_RTK_EXIT", "3")
	t.Setenv("FAKE_RTK_OUT", "rtk ls")
	if got, ok := rtkOut(t, rtkBashIn("ls")); !ok || !strings.HasSuffix(got, " ls") {
		t.Errorf("exit 3 = %q (emitted=%v), want a rewrite", got, ok)
	}
	bad := filepath.Join(t.TempDir(), "rtk") // present but not executable: a failure, not a rewrite
	if err := os.WriteFile(bad, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shell.RTKBinaryEnv, bad)
	if got, ok := rtkOut(t, rtkBashIn("ls")); ok {
		t.Errorf("unrunnable rtk rewrote to %q", got)
	}
}

// A bare `rtk` at any command position gets the pinned absolute bin; `echo rtk status` is an argument.
func TestRTKAbsoluteAtEveryCommandPosition(t *testing.T) {
	bin := fakeRTK(t)
	t.Setenv("FAKE_RTK_OUT", "rtk git log && rtk ls | rtk wc ; rtk a || rtk b\nrtk c; echo rtk status")
	got, ok := rtkOut(t, rtkBashIn("git log && ls | wc ; a || b\nc; echo x"))
	want := bin + " git log && " + bin + " ls | " + bin + " wc ; " + bin + " a || " + bin + " b\n" + bin + " c; echo rtk status"
	if !ok || got != want {
		t.Errorf("got %q (emitted=%v)\nwant %q", got, ok, want)
	}
}

func TestRTKPinnedBinaryShellMetacharacters(t *testing.T) {
	for _, name := range []string{"rtk;other", "rtk(other)", "rtk\nother", "rtk space", "rtk'quote", "rtk$dollar"} {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), name)
			script := "#!/bin/sh\nif [ \"$1\" = rewrite ]; then printf 'rtk ls'; else printf '%s' \"$1\"; fi\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv(shell.RTKBinaryEnv, bin)
			cmd, ok := rtkOut(t, rtkBashIn("ls"))
			if !ok {
				t.Fatal("expected rewrite using pinned binary")
			}
			out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
			if err != nil || string(out) != "ls" {
				t.Fatalf("rewritten command %q: output=%q error=%v, want pinned binary output ls", cmd, out, err)
			}
		})
	}
}

// The docs-plan-tick bug: a rewritten commit chain hides git's own output from the plan tick.
func TestRTKNeverRewritesGitCommit(t *testing.T) {
	fakeRTK(t)
	for _, cmd := range []string{`git add x && git commit -m y`, `git commit -m y`, `cd /tmp && git commit -am y`} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was rewritten to %q", cmd, got)
		}
	}
}

func TestRTKMissingBinaryIsNoOp(t *testing.T) {
	t.Setenv(shell.RTKBinaryEnv, "definitely-not-a-real-binary-xyz")
	if got, ok := rtkOut(t, rtkBashIn("git log")); ok {
		t.Errorf("missing binary still wrapped to %q — must degrade to a no-op", got)
	}
}

func TestRTKIgnoresNonBashTools(t *testing.T) {
	fakeRTK(t)
	b, _ := json.Marshal(map[string]any{
		"tool_name": "Read", "tool_input": map[string]any{"command": "git log"},
	})
	if got, ok := rtkOut(t, string(b)); ok {
		t.Errorf("non-Bash tool wrapped to %q", got)
	}
}

// rtkCwdIn builds a Bash payload carrying the hook input cwd.
func rtkCwdIn(cmd, cwd string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "cwd": cwd, "tool_input": map[string]any{"command": cmd},
	})
	return string(b)
}

// rtkWorktreeFixture builds a main checkout and a linked worktree (with a sub dir) whose .git points into main.
func rtkWorktreeFixture(t *testing.T) (main, wt string) {
	t.Helper()
	base := t.TempDir()
	main, wt = filepath.Join(base, "main"), filepath.Join(base, "wt")
	for _, d := range []string{filepath.Join(main, ".git", "worktrees", "x"), filepath.Join(wt, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ptr := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}
	return main, wt
}

// requireRTKWrapped asserts cmd run from cwd is rewritten to want.
func requireRTKWrapped(t *testing.T, label, cmd, cwd, want string) {
	t.Helper()
	if got, ok := rtkOut(t, rtkCwdIn(cmd, cwd)); !ok || got != want {
		t.Errorf("%s = %q (emitted=%v), want %q", label, got, ok, want)
	}
}

func TestRTKGitInLinkedWorktreeIsNotRewritten(t *testing.T) {
	bin := fakeRTK(t)
	main, wt := rtkWorktreeFixture(t)
	for _, cwd := range []string{wt, filepath.Join(wt, "sub")} {
		if got, ok := rtkOut(t, rtkCwdIn("git log --oneline -5", cwd)); ok {
			t.Errorf("git in worktree %q rewritten to %q", cwd, got)
		}
	}
	requireRTKWrapped(t, "npm in worktree", "npm ls", wt, bin+" npm ls")
	requireRTKWrapped(t, "git in main checkout", "git log --oneline -5", main, bin+" git log --oneline -5")
	for _, cmd := range []string{"cd " + wt + " && git status", "git -C " + wt + " status"} {
		if got, ok := rtkOut(t, rtkCwdIn(cmd, main)); ok {
			t.Errorf("%q from the main checkout rewritten to %q", cmd, got)
		}
	}
	requireRTKWrapped(t, "chain in main", "git log -1 && ls", main, bin+" git log -1 && ls")
}

// TestRTKChainParsingUsesShellHelpers: cd options, wrapped git and quoted separators read as the shell does.
func TestRTKChainParsingUsesShellHelpers(t *testing.T) {
	bin := fakeRTK(t)
	main, wt := rtkWorktreeFixture(t)
	for _, cmd := range []string{"cd -P " + wt + " && git status", "env -C " + wt + " git status",
		"sudo git rev-parse HEAD", "env -u X git rev-parse HEAD", "time git rev-parse HEAD"} {
		if got, ok := rtkOut(t, rtkCwdIn(cmd, main)); ok {
			t.Errorf("%q rewritten to %q, want it left alone", cmd, got)
		}
	}
	quoted := `echo 'a; cd ` + wt + `'&& git log -1`
	requireRTKWrapped(t, "quoted ; in echo", quoted, main, bin+" "+quoted)
}

// The safety gate judges safety and nothing else: with rtk available it must NOT rewrite the
// command — rewriting is the rtk group's job, in a separate process after the gate.
func TestRtkSplitSafetyScanNeverRewrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := fakeRTK(t)
	payload := rtkBashIn("git status")

	if res := scanPayload(strings.NewReader(payload)); res.Decision != core.Allow || res.UpdatedInput != nil || res.Context != "" {
		t.Fatalf("SafetyScan result %+v — it must never emit updatedInput; rewriting belongs to the rtk group", res)
	}

	got, ok := rtkOut(t, payload)
	if !ok || got != bin+" git status" {
		t.Fatalf("RTKRewrite command = %q (emitted=%v), want %q", got, ok, bin+" git status")
	}
}

// A bin under $HOME is spelled through "$HOME/…" so allow rules match on every machine; anything else stays absolute.
func TestRTKWordSpellsHome(t *testing.T) {
	if got := rtkWord("/home/u/.claude/bin/rtk", "/home/u", true); got != "rtk" {
		t.Errorf("PATH finds the pin: rtkWord = %q, want bare rtk", got)
	}
	for _, tc := range []struct{ name, bin, home, want string }{
		{"under home", "/home/u/.claude/bin/rtk", "/home/u", `"$HOME/.claude/bin/rtk"`},
		{"home trailing slash", "/home/u/.claude/bin/rtk", "/home/u/", `"$HOME/.claude/bin/rtk"`},
		{"outside home", "/usr/local/bin/rtk", "/home/u", "/usr/local/bin/rtk"},
		{"sibling prefix", "/home/user2/bin/rtk", "/home/u", "/home/user2/bin/rtk"},
		{"home unset", "/home/u/.claude/bin/rtk", "", "/home/u/.claude/bin/rtk"},
		{"home is root", "/usr/bin/rtk", "/", "/usr/bin/rtk"},
		{"specials escaped", "/home/u/a b\"$`\\/rtk", "/home/u", `"$HOME/a b\"\$\` + "`" + `\\/rtk"`},
	} {
		if got := rtkWord(tc.bin, tc.home, false); got != tc.want {
			t.Errorf("%s: rtkWord(%q, %q) = %q, want %q", tc.name, tc.bin, tc.home, got, tc.want)
		}
	}
}

// homeRTK installs the fake rtk under a fresh $HOME and returns its "$HOME/…" spelling.
func homeRTK(t *testing.T) string {
	t.Helper()
	bin := fakeRTK(t)
	t.Setenv("HOME", filepath.Dir(filepath.Dir(bin)))
	return `"$HOME/` + filepath.Base(filepath.Dir(bin)) + `/rtk"`
}

// End to end: the rewrite and the bypass note carry the $HOME spelling, the shell runs it, and it is never re-wrapped.
func TestRTKRewriteUsesHomeSpelling(t *testing.T) {
	word := homeRTK(t)
	in := core.ParseInput(strings.NewReader(rtkBashIn("ls internal")))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	res := RTKRewrite(in)
	if got := res.UpdatedInput["command"]; got != word+" ls internal" {
		t.Fatalf("command = %q, want %q", got, word+" ls internal")
	}
	if !strings.Contains(res.Context, "exact bytes: "+word+" bypass <cmd>") {
		t.Errorf("note %q does not name %s", res.Context, word)
	}
	if out, err := exec.Command("sh", "-c", word+" rewrite ok").CombinedOutput(); err != nil || string(out) != "rtk ok" {
		t.Errorf("sh ran %s: output=%q err=%v", word, out, err)
	}
	for _, cmd := range []string{word + " ls", "$HOME/" + strings.Trim(word, `"`)[len("$HOME/"):] + " ls"} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("already-wrapped %q was rewritten to %q", cmd, got)
		}
	}
}

// The safety gate must not refuse the "$HOME/…"-prefixed rtk command any more than the absolute one.
func TestRTKHomeSpellingPassesSafetyScan(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	word := homeRTK(t)
	for _, cmd := range []string{word + " ls internal", word + " git log --oneline -5"} {
		if res := scanPayload(strings.NewReader(rtkBashIn(cmd))); res.Decision != core.Allow {
			t.Errorf("%q: decision=%d reason=%q, want allow", cmd, res.Decision, res.Reason)
		}
	}
}

// Bare rtk only when PATH resolves to the pinned file itself (symlinks followed); a different rtk first keeps $HOME.
func TestRTKWordBareOnlyWhenPathFindsPin(t *testing.T) {
	word := homeRTK(t)
	bin := os.Getenv(shell.RTKBinaryEnv)
	link := filepath.Join(t.TempDir(), "rtk")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	other := fakeRTKDir(t)
	for _, tc := range []struct{ name, path, want string }{
		{"pin dir on PATH", filepath.Dir(bin), "rtk"},
		{"symlink to pin on PATH", filepath.Dir(link), "rtk"},
		{"other rtk first", other + ":" + filepath.Dir(bin), word},
		{"no rtk on PATH", t.TempDir(), word},
	} {
		t.Setenv("PATH", tc.path)
		_, same := shell.RTKOnPath(bin)
		if got := rtkWord(bin, os.Getenv("HOME"), same); got != tc.want {
			t.Errorf("%s: rtkWord = %q, want %q", tc.name, got, tc.want)
		}
	}
	t.Setenv("PATH", filepath.Dir(bin))
	if got, ok := rtkOut(t, rtkBashIn("ls")); !ok || got != "rtk ls" {
		t.Errorf("PATH finds the pin: command = %q (emitted=%v), want %q", got, ok, "rtk ls")
	}
}

// fakeRTKDir writes a second, distinct rtk executable and returns its directory.
func fakeRTKDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rtk"), []byte("#!/bin/sh\necho 'rtk 0.0.1'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// PATH shadowing the pin earns one notice per session naming the shadow and the fix; a PATH that finds the pin, none.
func TestRTKShadowNoticeOncePerSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := fakeRTK(t)
	t.Setenv("PATH", fakeRTKDir(t))
	run := func(session string) string {
		in := core.ParseInput(strings.NewReader(rtkBashIn("ls")))
		in.SessionID = session
		return RTKRewrite(in).Context
	}
	shadow := filepath.Join(os.Getenv("PATH"), "rtk")
	if got := run("s1"); !strings.Contains(got, "rtk on PATH is "+shadow+" (rtk 0.0.1), not the pinned binary — run make install") {
		t.Errorf("first call note %q lacks the shadow notice", got)
	}
	if got := run("s1"); strings.Contains(got, "run make install") {
		t.Errorf("second call in the same session repeated the notice: %q", got)
	}
	if got := run("s2"); !strings.Contains(got, "rtk on PATH is "+shadow) {
		t.Errorf("a new session got no notice: %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := run("s3"); !strings.Contains(got, "no rtk on PATH, so the pinned "+bin+" runs by path — run make install") {
		t.Errorf("no-rtk note %q lacks the missing notice", got)
	}
	t.Setenv("PATH", filepath.Dir(bin))
	if got := run("s4"); strings.Contains(got, "make install") {
		t.Errorf("PATH finds the pin, yet the note warns: %q", got)
	}
}
