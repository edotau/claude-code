package hooks

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rtkOut(t *testing.T, in string) (cmd string, emitted bool) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // the test process cwd sits inside the real config tree
	var sb strings.Builder
	if rc := RTKRewrite(strings.NewReader(in), &sb, io.Discard); rc != ExitProceed {
		t.Fatalf("rc = %d, want ExitProceed (%d) for this safe command", rc, ExitProceed)
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", false
	}
	var out struct {
		HookSpecificOutput struct {
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &out); err != nil {
		t.Fatalf("emitted non-JSON: %v\n%s", err, sb.String())
	}
	s, _ := out.HookSpecificOutput.UpdatedInput["command"].(string)
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
	t.Setenv(rtkBinaryEnv, bin)
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
	if vendor := ParseInput(strings.NewReader(payload)).Vendor(); vendor != VendorCodex {
		t.Fatalf("Vendor() = %q, want %q", vendor, VendorCodex)
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
	var stdout, stderr strings.Builder
	rc := SafetyScan(strings.NewReader(rtkBashIn("sudo apt install x")), &stdout, &stderr)
	if rc != ExitBlock {
		t.Fatalf("rc = %d, want ExitBlock (%d)", rc, ExitBlock)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "BLOCKED") {
		t.Fatalf("want stderr denial without Copilot JSON: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCopilotPayloadOverridesCodexVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_HOOK_AGENT", "codex")
	payload := `{"sessionId":"copilot","toolName":"bash","toolArgs":{"command":"sudo apt install x"}}`
	if vendor := ParseInput(strings.NewReader(payload)).Vendor(); vendor != VendorCopilot {
		t.Fatalf("Vendor() = %q, want %q", vendor, VendorCopilot)
	}
	var stdout strings.Builder
	if rc := SafetyScan(strings.NewReader(payload), &stdout, io.Discard); rc != ExitBlock {
		t.Fatalf("rc = %d, want ExitBlock (%d)", rc, ExitBlock)
	}
	var out struct {
		PermissionDecision string `json:"permissionDecision"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil {
		t.Fatal(err)
	}
	if out.PermissionDecision != "deny" {
		t.Fatalf("Copilot permission decision = %q, want deny", out.PermissionDecision)
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
	var sb strings.Builder
	RTKRewrite(strings.NewReader(rtkBashIn("npm install")), &sb, io.Discard)
	var out struct {
		HookSpecificOutput struct {
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	json.Unmarshal([]byte(sb.String()), &out)
	if out.HookSpecificOutput.UpdatedInput["description"] != "d" {
		t.Errorf("description dropped: %#v", out.HookSpecificOutput.UpdatedInput)
	}
	if out.HookSpecificOutput.UpdatedInput["timeout"] == nil {
		t.Errorf("timeout dropped: %#v", out.HookSpecificOutput.UpdatedInput)
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
	t.Setenv(rtkBinaryEnv, bad)
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
	t.Setenv(rtkBinaryEnv, "definitely-not-a-real-binary-xyz")
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

func TestRTKGitInLinkedWorktreeIsNotRewritten(t *testing.T) {
	bin := fakeRTK(t)
	base := t.TempDir()
	main, wt := filepath.Join(base, "main"), filepath.Join(base, "wt")
	for _, d := range []string{filepath.Join(main, ".git", "worktrees", "x"), filepath.Join(wt, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ptr := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{wt, filepath.Join(wt, "sub")} {
		if got, ok := rtkOut(t, rtkCwdIn("git log --oneline -5", cwd)); ok {
			t.Errorf("git in worktree %q rewritten to %q", cwd, got)
		}
	}
	if got, ok := rtkOut(t, rtkCwdIn("npm ls", wt)); !ok || got != bin+" npm ls" {
		t.Errorf("npm in worktree = %q (emitted=%v), want wrapped", got, ok)
	}
	if got, ok := rtkOut(t, rtkCwdIn("git log --oneline -5", main)); !ok || got != bin+" git log --oneline -5" {
		t.Errorf("git in main checkout = %q (emitted=%v), want wrapped", got, ok)
	}
	for _, cmd := range []string{"cd " + wt + " && git status", "git -C " + wt + " status"} {
		if got, ok := rtkOut(t, rtkCwdIn(cmd, main)); ok {
			t.Errorf("%q from the main checkout rewritten to %q", cmd, got)
		}
	}
	if got, ok := rtkOut(t, rtkCwdIn("git log -1 && ls", main)); !ok || got != bin+" git log -1 && ls" {
		t.Errorf("chain in main = %q (emitted=%v), want rewritten", got, ok)
	}
}
