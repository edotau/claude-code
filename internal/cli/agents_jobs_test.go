package cli

import (
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
