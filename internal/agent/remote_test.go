package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	req := Request{Model: hostile[0], Provider: hostile[1], Effort: hostile[2], SessionID: hostile[3], SystemPrompt: hostile[4],
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
		if _, err := RemoteArgv(host, "claude-code", "codex", "", Request{}); err == nil {
			t.Errorf("host %q accepted", host)
		}
	}
	if _, err := RemoteArgv("h", "claude-code", "codex", "", Request{Model: "a\x00b"}); err == nil {
		t.Error("NUL value accepted")
	}
	if _, err := RemoteArgv("h", "bin\nx", "codex", "", Request{}); err == nil {
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

// A remote that keeps reporting (retry notices on stderr) outlives timeout+grace; a silent one is still cut off.
func TestRemoteRunDeadlineIsPerActivityNotWholeRun(t *testing.T) {
	g := remoteGrace
	remoteGrace = 200 * time.Millisecond
	t.Cleanup(func() { remoteGrace = g })
	req := Request{Prompt: "p", Timeout: 200 * time.Millisecond}
	r := NewRemoteRunner("codex", "fakehost", "claude-code", "")

	fakeSSH(t, "for i in 1 2 3 4 5 6 7 8; do echo 'ask: transient, retrying' >&2; sleep 0.1; done\n"+
		`printf '%s\n' '{"schema":1,"type":"result","envelope":{"status":"ok","answer":"done"}}'`+"\n")
	res, err := r.Run(context.Background(), req, Discard)
	if err != nil || res.Answer != "done" {
		t.Fatalf("active remote killed: res=%+v err=%v", res, err)
	}

	fakeSSH(t, "exec sleep 5\n")
	start := time.Now()
	_, err = r.Run(context.Background(), req, Discard)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("silent remote: err=%v after %s, want a deadline well before 5s", err, time.Since(start))
	}
}

// A persona rides the single --agent value as vendor:persona.
func TestRemoteArgvPersonaIsOneAgentValue(t *testing.T) {
	script, err := remoteScript("claude-code", "codex", "", Request{Persona: &Persona{Name: "reviewer", Prompt: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(script, "--agent") != 1 || !strings.Contains(script, "--agent 'codex:reviewer'") || strings.Contains(script, "--persona") {
		t.Fatalf("script = %s, want one --agent 'codex:reviewer' and no --persona", script)
	}
}
