package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
)

func checkLostDispatches(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		m := readDispatch(t, p)
		if m["status"] != "error" || m["error"] != "lost: process gone" {
			t.Errorf("%s: status/error = %v/%v, want error/lost: process gone", filepath.Base(p), m["status"], m["error"])
		}
		if m["duration_ms"] != float64(3000) || m["agent"] != "codex" {
			t.Errorf("%s: other fields not preserved: %v", filepath.Base(p), m)
		}
	}
}

// deadPID returns the pid of a child that has already exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no true binary: %v", err)
	}
	return cmd.Process.Pid
}

func readDispatch(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func TestAgentsUsageFits80(t *testing.T) {
	var buf bytes.Buffer
	agentsPage.Fprint(&buf)
	usage := buf.String()
	for line := range strings.SplitSeq(usage, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("agentsUsage row is %d runes (>80): %q", n, line)
		}
	}
	if !strings.Contains(usage, "  reap") {
		t.Error("agentsPage must document reap")
	}
	code, _, stderr := captureOutput(t, func() int { return CmdAgents([]string{"reap", "-h"}) })
	if code != 0 || !strings.Contains(stderr, "--dry-run") {
		t.Errorf("agents reap help must document --dry-run: exit %d:\n%s", code, stderr)
	}
}

func TestCmdAgentsReapDryRunChangesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	dir := filepath.Join(paths.StateDir(), "agents-inflight")
	marker := filepath.Join(dir, "agent-"+strconv.Itoa(deadPID(t))+"-toolu_dead")
	writeFixtureFile(t, marker, "")
	before, _ := os.ReadFile(dead)

	code, stdout, stderr := captureOutput(t, func() int { return CmdAgents([]string{"reap", "--dry-run"}) })
	if code != 0 {
		t.Fatalf("reap --dry-run exit %d, stderr=%s", code, stderr)
	}
	if after, _ := os.ReadFile(dead); string(after) != string(before) {
		t.Errorf("--dry-run rewrote the record:\n%s", after)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("--dry-run removed a marker: %v", err)
	}
	if !strings.Contains(stdout, "000001") {
		t.Errorf("--dry-run should still print what it would change, got %q", stdout)
	}
}

func TestCmdAgentsReapMarkers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(paths.StateDir(), "agents-inflight")
	deadM := filepath.Join(dir, "agent-"+strconv.Itoa(deadPID(t))+"-toolu_dead")
	liveM := filepath.Join(dir, "agent-"+strconv.Itoa(os.Getpid())+"-toolu_live")
	lock := filepath.Join(dir, ".lock")
	stray := filepath.Join(dir, "notes.txt")
	for _, p := range []string{deadM, liveM, lock, stray} {
		writeFixtureFile(t, p, "")
	}
	if code, _, stderr := captureOutput(t, func() int { return CmdAgents([]string{"reap"}) }); code != 0 {
		t.Fatalf("reap exit %d, stderr=%s", code, stderr)
	}
	if _, err := os.Stat(deadM); !os.IsNotExist(err) {
		t.Errorf("dead-owner marker should be removed (err=%v)", err)
	}
	for _, p := range []string{liveM, lock, stray} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive reap: %v", filepath.Base(p), err)
		}
	}
}

func TestCmdAgentsReapMarksLostDispatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	live := writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), time.Hour)
	done := writeDispatch(t, "20261024T000003.000-codex", "ok", deadPID(t), time.Hour)
	young := writeDispatch(t, "20261024T000004.000-codex", "running", 0, 5*time.Second)
	old0 := writeDispatch(t, "20261024T000005.000-codex", "running", 0, 10*time.Minute)

	code, stdout, stderr := captureOutput(t, func() int { return CmdAgents([]string{"reap"}) })
	if code != 0 {
		t.Fatalf("reap exit %d, stderr=%s", code, stderr)
	}
	checkLostDispatches(t, []string{dead, old0})
	for _, p := range []string{live, young} {
		if m := readDispatch(t, p); m["status"] != "running" {
			t.Errorf("%s: alive/in-grace record was reaped: %v", filepath.Base(p), m["status"])
		}
	}
	if m := readDispatch(t, done); m["status"] != "ok" || m["error"] != nil {
		t.Errorf("finished record touched: %v", m)
	}
	if !strings.Contains(stdout, "000001") {
		t.Errorf("reap output should name the reaped dispatch, got %q", stdout)
	}
}

func TestCmdAgentsReapNothingToDo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code, _, stderr := captureOutput(t, func() int { return CmdAgents([]string{"reap"}) }); code != 0 {
		t.Fatalf("reap on empty state exit %d, stderr=%s", code, stderr)
	}
}

// --background and -bg are a superset of the default: a running dispatch from another repo stays listed.
func TestCmdAgentsTreeBackgroundKeepsRunningDispatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	for _, flag := range []string{"--background", "-bg"} {
		code, stdout, stderr := captureOutput(t, func() int { return CmdAgents([]string{"tree", flag, "--days", "1"}) })
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", flag, code, stderr)
		}
		if !strings.Contains(stdout, "000002") {
			t.Errorf("%s dropped the running dispatch:\n%s", flag, stdout)
		}
	}
}

// (i) CLI wiring: `agents tree --format json` resolves claudeDir from CLAUDE_CONFIG_DIR, dispatches
// through CmdAgents, and emits the documented {sessions:[...]} schema on stdout.
func TestCmdAgentsTreeFormatJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFixtureFile(t, filepath.Join(dir, "projects", "-Users-fake-repo", "sess0001.jsonl"),
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"hello"},"cwd":"/Users/fake/repo","gitBranch":"main"}`)

	code, stdout, stderr := captureOutput(t, func() int { return CmdAgents([]string{"tree", "--days", "365", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("agents tree --format json exit %d, stderr=%s", code, stderr)
	}
	var decoded struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal stdout: %v\nstdout: %s", err, stdout)
	}
	if len(decoded.Sessions) != 1 || decoded.Sessions[0].ID != "sess0001" {
		t.Errorf("sessions = %+v, want one session sess0001", decoded.Sessions)
	}
}

// agents tree rejects an unknown --format value before touching the filesystem.
func TestCmdAgentsTreeRejectsBadFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return CmdAgents([]string{"tree", "--format", "xml"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stderr == "" {
		t.Error("expected an error on stderr for an unknown --format value")
	}
}

// agents tree --running lists running dispatches machine-wide, including other repos, ignoring --days.
func TestCmdAgentsTreeRunningListsDispatchesMachineWide(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	writeDispatch(t, "20261024T000003.000-codex", "ok", os.Getpid(), time.Minute)
	code, stdout, stderr := captureOutput(t, func() int { return CmdAgents([]string{"tree", "--days", "1"}) })
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "000002") {
		t.Errorf("running dispatch from another repo missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "000003") {
		t.Errorf("finished dispatch listed by default:\n%s", stdout)
	}
}

// agents tree's text output (the default) renders without error when no sessions are in scope.
func TestCmdAgentsTreeTextNoSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := captureOutput(t, func() int { return CmdAgents([]string{"tree"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout == "" {
		t.Error("expected a \"no sessions in scope\" message on stdout")
	}
}

// agents tree --watch rejects a zero or negative --interval instead of busy-looping.
func TestCmdAgentsTreeWatchRejectsNonPositiveInterval(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, iv := range []string{"0", "-1s"} {
		code, _, stderr := captureOutput(t, func() int { return CmdAgents([]string{"tree", "--watch", "--interval", iv}) })
		if code != 2 || stderr == "" {
			t.Errorf("--interval %s: exit %d stderr %q, want exit 2 with a message", iv, code, stderr)
		}
	}
}

func writeDispatch(t *testing.T, id, status string, pid int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), "dispatch", id+".json")
	rec := map[string]any{"id": id, "time": time.Now().Add(-age).UTC().Format(time.RFC3339Nano), "agent": "codex",
		"request": map[string]any{}, "status": status, "pid": pid, "repo": "/some/other/repo", "duration_ms": 3000}
	b, _ := json.Marshal(rec)
	writeFixtureFile(t, p, string(b))
	return p
}

// writeFixtureFile creates path (with parents) holding content.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
