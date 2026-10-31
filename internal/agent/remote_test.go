package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The remote command line is run by a real sh against a stub binary that prints its argv, one NUL-terminated word each.
func TestRemoteArgvSurvivesHostileValues(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	stub := filepath.Join(dir, "cc stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := []string{"a'b", `a"b`, "$(touch " + marker + ")", "`touch " + marker + "`", "x; touch " + marker, "l1\nl2 'q' \\", "-oProxyCommand=x", "tab\there"}
	req := RunOptions{Model: hostile[0], Provider: hostile[1], Effort: hostile[2], SessionID: hostile[3], SystemPrompt: hostile[4],
		AppendSystemPrompt: hostile[5], OutputSchema: hostile[6], WorkDir: "/w/" + hostile[7], Env: []string{"K=" + hostile[0] + hostile[5]},
		Timeout: time.Minute, Tools: true}
	argv, err := RemoteArgv("wks", stub, "codex", "", req)
	if err != nil {
		t.Fatal(err)
	}
	if got := argv[len(argv)-3:]; got[0] != "--" || got[1] != "wks" {
		t.Fatalf("host must follow --, got %q", got)
	}
	out, err := exec.Command("sh", "-c", argv[len(argv)-1]).Output()
	if err != nil {
		t.Fatalf("remote line failed: %v", err)
	}
	want := []string{"ask", "--format", "stream-json", "--fallback", "none", "--stdin-framed", "--agent", "codex", "--model", hostile[0], "--profile", hostile[1],
		"--effort", hostile[2], "--session", hostile[3], "--system-prompt", hostile[4], "--append-system-prompt", hostile[5],
		"--output-schema", hostile[6], "--workdir", "/w/" + hostile[7], "--timeout", "1m0s", "--env", "K=" + hostile[0] + hostile[5], "--tools"}
	if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !slices.Equal(got, want) {
		t.Fatalf("argv mangled:\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hostile value executed on the remote")
	}
	if strings.Contains(strings.Join(argv, " "), "ask") {
		t.Fatal("the ssh argv leaks the plain command line instead of the encoded one")
	}
}

func TestRemoteArgvRefusals(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=x", "", "a b", "h\n"} {
		if _, err := RemoteArgv(host, "claude-code", "codex", "", RunOptions{}); err == nil {
			t.Errorf("host %q accepted", host)
		}
	}
	if _, err := RemoteArgv("h", "claude-code", "codex", "", RunOptions{Model: "a\x00b"}); err == nil {
		t.Error("NUL value accepted")
	}
	if _, err := RemoteArgv("h", "bin\nx", "codex", "", RunOptions{}); err == nil {
		t.Error("newline in remote bin accepted")
	}
}

// fakeSSH puts an `ssh` script with the given body on PATH, in hermetic config dirs.
func fakeSSH(t *testing.T, body string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// handTimer is a watchdog the test drives: it counts Resets and fires only when told to.
type handTimer struct {
	mu     sync.Mutex
	resets int
	f      func()
}

func (h *handTimer) Reset(time.Duration) bool { h.mu.Lock(); h.resets++; h.mu.Unlock(); return true }
func (h *handTimer) Stop() bool               { return true }
func (h *handTimer) count() int               { h.mu.Lock(); defer h.mu.Unlock(); return h.resets }
func (h *handTimer) fire()                    { h.mu.Lock(); f := h.f; h.mu.Unlock(); f() }

// swapIdleTimer installs a handTimer for the test and returns it.
func swapIdleTimer(t *testing.T) *handTimer {
	h := &handTimer{}
	orig := newIdleTimer
	newIdleTimer = func(_ time.Duration, f func()) idleTimer { h.mu.Lock(); h.f = f; h.mu.Unlock(); return h }
	t.Cleanup(func() { newIdleTimer = orig })
	return h
}

// Every remote line resets the watchdog, so an active remote is never cut off; a silent one is once the watchdog fires.
func TestRemoteRunDeadlineIsPerActivityNotWholeRun(t *testing.T) {
	h := swapIdleTimer(t)
	req := RunOptions{Prompt: "p", Timeout: time.Minute}
	r := NewRemoteRunner("codex", "fakehost", "claude-code", "")

	fakeSSH(t, "for i in 1 2 3 4 5 6 7 8; do echo 'ask: transient, retrying' >&2; done\n"+
		`printf '%s\n' '{"schema":1,"type":"result","envelope":{"status":"ok","answer":"done"}}'`+"\n")
	res, err := r.Run(context.Background(), req, Discard)
	if err != nil || res.Answer != "done" {
		t.Fatalf("active remote killed: res=%+v err=%v", res, err)
	}
	if got := h.count(); got < 1 {
		t.Fatalf("watchdog never reset by remote output (%d resets): the deadline is not per-activity", got)
	}

	ready := filepath.Join(t.TempDir(), "ready")
	fakeSSH(t, "touch "+ready+"\nexec sleep 30\n")
	go func() {
		for _, err := os.Stat(ready); err != nil; _, err = os.Stat(ready) {
			time.Sleep(5 * time.Millisecond)
		}
		h.fire()
	}()
	_, err = r.Run(context.Background(), req, Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("silent remote: err=%v, want the watchdog deadline", err)
	}
}

// A persona rides the single --agent value as vendor:persona.
func TestRemoteArgvPersonaIsOneAgentValue(t *testing.T) {
	script, err := remoteScript("claude-code", "codex", "", RunOptions{Persona: &Schema{Name: "reviewer", Prompt: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(script, "--agent") != 1 || !strings.Contains(script, "--agent 'codex:reviewer'") || strings.Contains(script, "--persona") {
		t.Fatalf("script = %s, want one --agent 'codex:reviewer' and no --persona", script)
	}
}

// ssh's non-login shell has no harness PATH: a bare claude-code must still resolve from ~/.local/bin.
func TestRemoteScriptResolvesHarnessPath(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-code"), []byte("#!/bin/sh\necho resolved\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := remoteScript("claude-code", "codex", "", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "resolved" {
		t.Fatalf("script %q = %q, %v; want the ~/.local/bin claude-code", script, out, err)
	}
}

// A local absolute path must not reach the remote: its home is a different directory.
func TestRemoteHomePath(t *testing.T) {
	t.Setenv("HOME", "/Users/me")
	for in, want := range map[string]string{
		"/Users/me":          "~",
		"/Users/me/.claude":  "~/.claude",
		"/Users/me/a/../b":   "~/b",
		"/Users/mallory/x":   "/Users/mallory/x",
		"/Users/me/../other": "/Users/me/../other",
		"/srv/repo":          "/srv/repo",
		"~/.claude":          "~/.claude",
		"relative/dir":       "relative/dir",
		"":                   "",
	} {
		if got := remoteHomePath(in); got != want {
			t.Errorf("remoteHomePath(%q) = %q, want %q", in, got, want)
		}
	}
	script, err := remoteScript("/Users/me/.local/bin/claude-code", "codex", "", RunOptions{WorkDir: "/Users/me/.claude"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "/Users/me") || !strings.Contains(script, `"$HOME"/'.claude'`) {
		t.Fatalf("local home leaked into the remote script: %s", script)
	}
}
