package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runCLI drives the real dispatcher: Run(["claude-code", args...]).
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return Run(append([]string{"claude-code"}, args...)) })
}

// Every retired spelling exits 2 with the usage error, never an alias, a shim or a forward to the agent CLI.
func TestRetiredSpellingsExitTwo(t *testing.T) {
	vaulttest.UseFreshKey(t)
	cfgDir(t)
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"agents", "tail", "x"}, "unknown subcommand"},
		{[]string{"agents", "reap"}, "unknown subcommand"},
		{[]string{"codex", "status"}, "unknown subcommand"},
		{[]string{"codex", "tail", "-f"}, "unknown subcommand"},
		{[]string{"gemini", "result", "x"}, "unknown subcommand"},
		{[]string{"copilot", "cancel"}, "unknown subcommand"},
		{[]string{"opencode", "resume", "x", "task"}, "unknown subcommand"},
		{[]string{"codex", "on"}, "unknown subcommand"},
		{[]string{"codex", "off"}, "unknown subcommand"},
		{[]string{"agents", "tree", "--background"}, "flag provided but not defined"},
		{[]string{"agents", "tree", "--bg"}, "flag provided but not defined"},
		{[]string{"agents", "label", "Plan", "--brief", "f"}, "flag provided but not defined"},
		{[]string{"agents", "sync", "--no-prune"}, "flag provided but not defined"},
		{[]string{"agents", "list", "--host-bin", "x"}, "flag provided but not defined"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if code, _, stderr := runCLI(t, tc.args...); code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want exit 2 with %q", code, stderr, tc.want)
			}
		})
	}
}

// seedCodexJob saves a finished codex record in this repo whose log holds body.
func seedCodexJob(t *testing.T, body string) agent.Record {
	t.Helper()
	rec := saveResultRecord(t, "ok", "")
	rec.Log = filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(rec.Log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

// Bare status lists recent dispatches of every agent; --agent narrows to one agent; an id shows one dispatch.
func TestAgentsStatusForms(t *testing.T) {
	cfgDir(t)
	if out := okAgents(t, "status", "--agent", "codex"); !strings.Contains(out, "no codex dispatches logged yet") {
		t.Errorf("empty --agent list = %q, want the empty text", out)
	}
	codex, claude := seedCodexJob(t, "x\n"), seedClaudeJob(t)
	for _, tc := range []struct {
		name string
		args []string
		has  []string
		not  []string
	}{
		{"bare lists every agent", []string{"status"}, []string{codex.ID, claude.ID}, nil},
		{"--agent narrows", []string{"status", "--agent", "codex"}, []string{codex.ID}, []string{claude.ID}},
		{"--agent --all", []string{"status", "--agent", "claude", "--all"}, []string{claude.ID}, []string{codex.ID}},
		{"id alone", []string{"status", claude.ID}, []string{claude.ID, "ok"}, []string{codex.ID}},
		{"id with --agent", []string{"status", "--agent", "codex", codex.ID}, []string{codex.ID}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := okAgents(t, tc.args...)
			for _, w := range tc.has {
				if !strings.Contains(out, w) {
					t.Errorf("status %v lacks %q:\n%s", tc.args, w, out)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(out, w) {
					t.Errorf("status %v must not list %q:\n%s", tc.args, w, out)
				}
			}
		})
	}
	for _, args := range [][]string{{"status", "--all"}, {"status", "a", "b"}, {"status", "--format", "xml"}} {
		if code, _, _ := runAgents(t, args...); code != 2 {
			t.Errorf("agents %v exit %d, want 2", args, code)
		}
	}
}

func TestAgentsStatusQueued(t *testing.T) {
	cfgDir(t)
	r := seedCodexJob(t, "")
	r.Status, r.PID = "running", os.Getpid()
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	agent.NoteQueued(r.ID, "qa", nil)
	for _, args := range [][]string{
		{"status"}, {"status", r.ID}, {"status", "--agent", "codex", "--all"},
		{"status", "--format", "json"}, {"status", r.ID, "--format", "json"},
	} {
		out := okAgents(t, args...)
		if !strings.Contains(out, "queued") || !strings.Contains(out, "queued on qa, waiting ") {
			t.Errorf("status %v missing queue detail: %s", args, out)
		}
	}
	saved, err := agent.LoadRecord(r.ID)
	if err != nil || saved.Status != "running" {
		t.Fatalf("status display changed stored lifecycle: %+v, %v", saved, err)
	}
}

func TestAgentsStatusDeadQueuedJSON(t *testing.T) {
	pid := deadPID(t)
	queued := "queued " + time.Now().Add(-time.Minute).Format(time.RFC3339Nano) + " qa"
	for _, tc := range []struct {
		status, progress, wantStatus, wantProgress string
	}{
		{"running", queued, "lost", ""},
		{"error", queued, "error", ""},
		{"ok", queued, "ok", ""},
		{"running", "part 2/3", "lost", "part 2/3"},
		{"error", "part 2/3", "error", "part 2/3"},
	} {
		t.Run(tc.status+"/"+tc.progress, func(t *testing.T) {
			cfgDir(t)
			r := seedCodexJob(t, "")
			r.Status, r.PID, r.Progress = tc.status, pid, tc.progress
			r.Time = time.Now().Add(-time.Hour)
			if err := r.Save(); err != nil {
				t.Fatal(err)
			}
			out := okAgents(t, "status", "--format", "json")
			var recs []agent.Record
			if err := json.Unmarshal([]byte(out), &recs); err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 {
				t.Fatalf("got %d records, want 1: %s", len(recs), out)
			}
			if got := recs[0]; got.ID != r.ID || got.Status != tc.wantStatus || got.Progress != tc.wantProgress {
				t.Errorf("status = %s, want status %q, progress %q", out, tc.wantStatus, tc.wantProgress)
			}
			if saved, err := agent.LoadRecord(r.ID); err != nil || saved.Progress != r.Progress {
				t.Fatalf("status changed stored progress: %+v, %v", saved, err)
			}
		})
	}
}

// `result -f` on a finished job prints its log once and the ask trailer; no id needs --agent (the newest job here).
func TestAgentsResultForms(t *testing.T) {
	cfgDir(t)
	rec := seedCodexJob(t, "line one\nline two\n")
	if out := okAgents(t, "result", rec.ID, "-f"); strings.Count(out, "line one") != 1 {
		t.Errorf("result -f printed the log %d times:\n%s", strings.Count(out, "line one"), out)
	}
	if out := okAgents(t, "result", "--agent", "codex"); !strings.Contains(out, "line two") {
		t.Errorf("result --agent codex = %q, want the newest job's log", out)
	}
	if out := okAgents(t, "result", "--agent", "codex", "-f"); !strings.Contains(out, "line two") {
		t.Errorf("result --agent codex -f = %q, want the newest job's log", out)
	}
	for name, args := range map[string][]string{
		"no id and no --agent": {"result"},
		"--save without id":    {"result", "--agent", "codex", "--save", filepath.Join(t.TempDir(), "o")},
		"--verdict without id": {"result", "--agent", "codex", "--verdict"},
		"-f with --verdict":    {"result", rec.ID, "-f", "--verdict"},
		"--events with --save": {"result", rec.ID, "--events", "--save", filepath.Join(t.TempDir(), "o")},
		"--events with json":   {"result", rec.ID, "--events", "--format", "json"},
		"two ids":              {"result", rec.ID, rec.ID},
	} {
		if code, _, _ := runAgents(t, args...); code != 2 {
			t.Errorf("%s: agents %v exit %d, want 2", name, args, code)
		}
	}
}

// `cancel --lost` is the old reap and refuses everything else; cancel without --lost needs an id or --agent.
func TestAgentsCancelForms(t *testing.T) {
	cfgDir(t)
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	before, _ := os.ReadFile(dead)
	if out := okAgents(t, "cancel", "--lost", "--dry-run"); !strings.Contains(out, "000001") {
		t.Errorf("--lost --dry-run = %q, want the lost dispatch named", out)
	}
	if after, _ := os.ReadFile(dead); string(after) != string(before) {
		t.Errorf("--dry-run rewrote the record:\n%s", after)
	}
	for name, args := range map[string][]string{
		"nothing":             {"cancel"},
		"--dry-run alone":     {"cancel", "--dry-run", "x"},
		"--lost with an id":   {"cancel", "--lost", "x"},
		"--lost with --agent": {"cancel", "--lost", "--agent", "codex"},
		"--lost with json":    {"cancel", "--lost", "--format", "json"},
	} {
		if code, _, _ := runAgents(t, args...); code != 2 {
			t.Errorf("%s: agents %v exit %d, want 2", name, args, code)
		}
	}
	if code, _, stderr := runAgents(t, "cancel", "--agent", "gemini"); code != 1 || stderr == "" {
		t.Errorf("cancel --agent with no running job = exit %d stderr %q, want exit 1 with a message", code, stderr)
	}
}

// profile's three selectors are exclusive.
func TestAgentProfileSelectorsExclusive(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, args := range [][]string{{"--clear", "--off"}, {"--off", "personal"}, {"--clear", "personal"}} {
		if code, _, stderr := profileCmd(t, "copilot", args...); code == 0 || !strings.Contains(stderr, "want one profile name") {
			t.Errorf("profile %v = exit %d stderr %q, want a refusal", args, code, stderr)
		}
	}
}
