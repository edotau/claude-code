package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	_, stdout, _ := hosttest.CaptureOutput(t, func() int { fn(); return 0 })
	return stdout
}

// cfgDir points CLAUDE_CONFIG_DIR at a fresh temp dir and returns it.
func cfgDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

// runAgents runs cmdAgents and returns its exit code, stdout and stderr.
func runAgents(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return cmdAgents(args) })
}

// okAgents runs cmdAgents, fails unless it exits 0, and returns stdout.
func okAgents(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := runAgents(t, args...)
	if code != 0 {
		t.Fatalf("agents %v: exit %d, stderr=%s", args, code, stderr)
	}
	return stdout
}

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

// The agents page documents reap and reap -h its --dry-run (line widths: TestHelpFormat).
func TestAgentsHelpDocumentsReap(t *testing.T) {
	var buf bytes.Buffer
	agentsPage.Fprint(&buf)
	if !strings.Contains(buf.String(), "  reap") {
		t.Error("agentsPage must document reap")
	}
	code, _, stderr := runAgents(t, "reap", "-h")
	if code != 0 || !strings.Contains(stderr, "--dry-run") {
		t.Errorf("agents reap help must document --dry-run: exit %d:\n%s", code, stderr)
	}
}

func TestCmdAgentsReapDryRunChangesNothing(t *testing.T) {
	cfgDir(t)
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	marker := filepath.Join(paths.StateDir(), "agents-inflight", "agent-"+strconv.Itoa(deadPID(t))+"-toolu_dead")
	hosttest.WriteRel(t, filepath.Dir(marker), filepath.Base(marker), "")
	before, _ := os.ReadFile(dead)

	stdout := okAgents(t, "reap", "--dry-run")
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
	cfgDir(t)
	dir := filepath.Join(paths.StateDir(), "agents-inflight")
	deadM := filepath.Join(dir, "agent-"+strconv.Itoa(deadPID(t))+"-toolu_dead")
	survivors := []string{
		filepath.Join(dir, "agent-"+strconv.Itoa(os.Getpid())+"-toolu_live"),
		filepath.Join(dir, ".lock"),
		filepath.Join(dir, "notes.txt"),
	}
	for _, p := range append([]string{deadM}, survivors...) {
		hosttest.WriteRel(t, filepath.Dir(p), filepath.Base(p), "")
	}
	okAgents(t, "reap")
	if _, err := os.Stat(deadM); !os.IsNotExist(err) {
		t.Errorf("dead-owner marker should be removed (err=%v)", err)
	}
	for _, p := range survivors {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive reap: %v", filepath.Base(p), err)
		}
	}
}

func TestCmdAgentsReapMarksLostDispatches(t *testing.T) {
	cfgDir(t)
	dead := writeDispatch(t, "20261024T000001.000-codex", "running", deadPID(t), time.Hour)
	live := writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), time.Hour)
	done := writeDispatch(t, "20261024T000003.000-codex", "ok", deadPID(t), time.Hour)
	young := writeDispatch(t, "20261024T000004.000-codex", "running", 0, 5*time.Second)
	old0 := writeDispatch(t, "20261024T000005.000-codex", "running", 0, 10*time.Minute)

	stdout := okAgents(t, "reap")
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
	cfgDir(t)
	okAgents(t, "reap")
}

// --background and -bg are a superset of the default: a running dispatch from another repo stays listed.
func TestCmdAgentsTreeBackgroundKeepsRunningDispatches(t *testing.T) {
	cfgDir(t)
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	for _, flag := range []string{"--background", "-bg"} {
		if stdout := okAgents(t, "tree", flag, "--days", "1"); !strings.Contains(stdout, "000002") {
			t.Errorf("%s dropped the running dispatch:\n%s", flag, stdout)
		}
	}
}

// `agents tree --format json` reads CLAUDE_CONFIG_DIR and emits the documented {sessions:[...]} schema.
func TestCmdAgentsTreeFormatJSON(t *testing.T) {
	dir := cfgDir(t)
	hosttest.WriteRel(t, dir, "projects/-Users-fake-repo/sess0001.jsonl",
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"hello"},"cwd":"/Users/fake/repo","gitBranch":"main"}`)

	stdout := okAgents(t, "tree", "--days", "365", "--format", "json")
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

// An unknown --format and a non-positive --watch --interval are refused (exit 2) instead of running.
func TestCmdAgentsTreeRejectsBadFlags(t *testing.T) {
	cfgDir(t)
	for name, args := range map[string][]string{
		"unknown format": {"tree", "--format", "xml"},
		"interval 0":     {"tree", "--watch", "--interval", "0"},
		"interval -1s":   {"tree", "--watch", "--interval", "-1s"},
	} {
		if code, _, stderr := runAgents(t, args...); code != 2 || stderr == "" {
			t.Errorf("%s: exit %d stderr %q, want exit 2 with a message", name, code, stderr)
		}
	}
}

// agents tree --running lists running dispatches machine-wide, including other repos, ignoring --days.
func TestCmdAgentsTreeRunningListsDispatchesMachineWide(t *testing.T) {
	cfgDir(t)
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	writeDispatch(t, "20261024T000003.000-codex", "ok", os.Getpid(), time.Minute)
	stdout := okAgents(t, "tree", "--days", "1")
	if !strings.Contains(stdout, "000002") {
		t.Errorf("running dispatch from another repo missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "000003") {
		t.Errorf("finished dispatch listed by default:\n%s", stdout)
	}
}

// The default text output renders a message when no sessions are in scope.
func TestCmdAgentsTreeTextNoSessions(t *testing.T) {
	cfgDir(t)
	if stdout := okAgents(t, "tree"); stdout == "" {
		t.Error("expected a \"no sessions in scope\" message on stdout")
	}
}

func writeDispatch(t *testing.T, id, status string, pid int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), "dispatch", id+".json")
	rec := map[string]any{"id": id, "time": time.Now().Add(-age).UTC().Format(time.RFC3339Nano), "agent": "codex",
		"request": map[string]any{}, "status": status, "pid": pid, "repo": "/some/other/repo", "duration_ms": 3000}
	b, _ := json.Marshal(rec)
	hosttest.WriteRel(t, filepath.Dir(p), filepath.Base(p), string(b))
	return p
}

// The core sub-verbs stay routed (presence, not exact membership): a renamed or dropped one silently stops routing.
func TestVendorSubverbsCoverContractVerbs(t *testing.T) {
	for _, w := range []string{"resume", "status", "result", "cancel", "tail", "setup", "catalog", "review", "transfer"} {
		if _, ok := agentSubPages[w]; !ok {
			t.Errorf("vendorSubverbs missing %q", w)
		}
	}
}

// notAVendor is an unregistered agent: every lookup rejects it in-process, so routing never reaches a vendor binary.
const notAVendor = "zzz-not-a-vendor"

func agentVerb(name string, args []string) int { return cmdAgentVerb(name, args) }

// Each sub-verb reaches its own handler (proven by that handler's message); anything else launches.
func TestVendorVerbRouting(t *testing.T) {
	for _, tc := range []struct {
		name, vendor string
		args         []string
		code         int // -1: not asserted
		onStdout     bool
		prefix       bool
		want, not    string
	}{
		{"double dash forces launch", notAVendor, []string{"--", "status"}, -1, false, false, "unknown agent", notAVendor + " status:"},
		{"status", notAVendor, []string{"status"}, -1, false, false, notAVendor + " status:", ""},
		{"result", notAVendor, []string{"result"}, -1, false, false, notAVendor + " result:", ""},
		{"cancel", notAVendor, []string{"cancel"}, -1, false, false, notAVendor + " cancel:", ""},
		{"tail", notAVendor, []string{"tail"}, -1, false, false, notAVendor + " tail:", ""},
		{"bare word launches", notAVendor, []string{"--native-flag", "x"}, -1, false, false, "unknown agent", ""},
		{"setup", notAVendor, []string{"setup"}, -1, true, false, notAVendor + "  agent  fail", ""},
		{"resume without id", "codex", []string{"resume"}, -1, false, false, "codex resume: usage:", ""},
		{"catalog extra arg", "codex", []string{"catalog", "unexpected"}, 2, false, false, "codex catalog: unexpected argument", ""},
		{"catalog help", "codex", []string{"catalog", "--help"}, 0, false, true, "usage: claude-code codex catalog", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(tc.vendor, tc.args) })
			got := stderr
			if tc.onStdout {
				got = stdout
			}
			if tc.code >= 0 && code != tc.code {
				t.Errorf("exit %d, want %d (stderr %q)", code, tc.code, stderr)
			}
			if ok := strings.Contains(got, tc.want); tc.prefix {
				ok = strings.HasPrefix(got, tc.want)
				if !ok {
					t.Errorf("output %q lacks prefix %q", got, tc.want)
				}
			} else if !ok {
				t.Errorf("output %q lacks %q", got, tc.want)
			}
			if tc.not != "" && strings.Contains(got, tc.not) {
				t.Errorf("output %q must not contain %q", got, tc.not)
			}
		})
	}
}

// Vendor help uses the shared page and exits successfully.
func TestVendorVerbHelpFlag(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help", "help"} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{flagArg}) })
		if code != 0 {
			t.Errorf("vendorVerb(codex, %q) exit %d, want 0", flagArg, code)
		}
		if !strings.HasPrefix(stderr, "usage: claude-code codex") {
			t.Errorf("vendorVerb(codex, %q) stderr missing usage header: %q", flagArg, stderr)
		}
		for _, want := range []string{"  catalog", "select the model for this launch"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("vendorVerb(codex, %q) help missing %q: %s", flagArg, want, stderr)
			}
		}
	}
}

// A blurb without a conversion must render verbatim, not with a %!(EXTRA ...) tail.
func TestAgentSubPageHasNoFormatDebris(t *testing.T) {
	for verb := range agentSubPages {
		if about := agentSubPage("codex", verb).About; strings.Contains(about, "%!") || strings.Contains(about, "%s") {
			t.Errorf("%s about = %q", verb, about)
		}
	}
}

func saveResultRecord(t *testing.T, status, errMsg string) agent.Record {
	t.Helper()
	cwd, _ := os.Getwd()
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.Status, rec.Error = status, errMsg
	rec.Repo = agent.RepoRoot(context.Background(), cwd)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVendorResultRecordExit(t *testing.T) {
	for _, tc := range []struct {
		name, status, errMsg string
		flags                []string
		code                 int
	}{
		{"error record", "error", "boom happened", nil, 1},
		{"error record json", "error", "boom happened", []string{"--format", "json"}, 1},
		{"ok record", "ok", "", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir(t)
			rec := saveResultRecord(t, tc.status, tc.errMsg)
			args := append(slices.Clone(tc.flags), rec.ID)
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentResult("codex", args) })
			if code != tc.code || strings.Contains(stderr, "error: boom happened") != (tc.code == 1) ||
				(tc.code == 0 && strings.Contains(stderr, "error:")) {
				t.Errorf("exit %d stderr %q, want exit %d (error line only when 1)", code, stderr, tc.code)
			}
		})
	}
}

// seedTailJob saves a finished codex record whose log holds body and returns its id.
func seedTailJob(t *testing.T, status, body string) string {
	t.Helper()
	cfgDir(t)
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Status, rec.Log = cwd, status, logPath
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func TestVendorTail(t *testing.T) {
	for _, tc := range []struct {
		name, status, body, id string // id "" tails the seeded job
		code                   int
		stdout, stderr         string // stdout is exact when set, stderr a substring
		stdoutHas              string
	}{
		{"finished ok job prints its log", "ok", "line one\nline two\n", "", 0, "line one\nline two\n", "", ""},
		{"error job prints log and exits 1", "error", "boom\n", "", 1, "", "", "boom"},
		{"unknown id fails with prefix", "ok", "x\n", "zzzzzzzz", 1, "", "codex tail:", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seedTailJob(t, tc.status, tc.body)
			if tc.id != "" {
				id = tc.id
			}
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
			if code != tc.code || (tc.stdout != "" && stdout != tc.stdout) || !strings.Contains(stdout, tc.stdoutHas) ||
				!strings.Contains(stderr, tc.stderr) {
				t.Errorf("tail = exit %d stdout %q stderr %q, want exit %d stdout %q%q stderr %q",
					code, stdout, stderr, tc.code, tc.stdout, tc.stdoutHas, tc.stderr)
			}
		})
	}
}

// partArgs is n repeated --part flags followed by tail.
func partArgs(n int, tail ...string) []string {
	var args []string
	for range n {
		args = append(args, "--part", "q")
	}
	return append(args, tail...)
}

// Bad dispatch args exit 2 and the message names the cause where one is pinned.
func TestDispatchUsageErrorsExit2(t *testing.T) {
	vaulttest.UseFreshKey(t)
	capMsg := fmt.Sprintf("%d --part", roles.MaxParts+1)
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"missing task", []string{"explore"}, nil},
		{"no args", []string{}, nil},
		{"unknown role", []string{"nonesuch", "task"}, []string{`unknown role "nonesuch"`}},
		{"over-cap --part", partArgs(roles.MaxParts+1, "explore", "task"), []string{capMsg, fmt.Sprintf("cap is %d", roles.MaxParts)}},
		{"bad schema", []string{"--output-schema", "{not json", "fleet", "task"}, []string{"--output-schema"}},
		{"schema file", []string{"--output-schema", "/nonexistent/schema.json", "fleet", "task"}, nil},
	} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsDispatch(tc.args) })
		if code != 2 {
			t.Errorf("%s: exit %d, want 2", tc.name, code)
		}
		for _, w := range tc.want {
			if !strings.Contains(stderr, w) {
				t.Errorf("%s: stderr %q lacks %q", tc.name, stderr, w)
			}
		}
	}
}

// writeEvents writes evs as JSON lines to the record's events file.
func writeEvents(t *testing.T, rec agent.Record, evs []wire.Event) {
	t.Helper()
	var b strings.Builder
	for _, ev := range evs {
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkJobTailEvents(t *testing.T, code int, out string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("tail = %d, %d lines %q, want 3 events then exit", code, len(lines), out)
	}
	for i, want := range []string{wire.EventStart, wire.EventText, wire.EventResult} {
		var ev wire.Event
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil || ev.Type != want {
			t.Errorf("line %d = %q, want type %s", i, lines[i], want)
		}
	}
}

// seedClaudeJob saves a finished claude record, an hour old, with a log and no events file.
func seedClaudeJob(t *testing.T) agent.Record {
	t.Helper()
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "say hi"}, time.Now().Add(-time.Hour))
	rec.Repo = t.TempDir()
	rec.Status, rec.SessionID, rec.DurationMs = "ok", "sess-1", 42
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.LogPath(), []byte("hi there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return rec
}

// jobResultJSON runs `agents result <id> --format json` and decodes the envelope.
func jobResultJSON(t *testing.T, id string) (wire.Envelope, string) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsJob("result", []string{id, "--format", "json"}) })
	var got wire.Envelope
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, stdout, err)
	}
	return got, stderr
}

func TestAgentsCancelNotRunning(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	if code := cmdAgentsJob("cancel", []string{rec.ID}); code == 0 {
		t.Error("cancel of a finished job exited 0, want an error")
	}
}

// status/result/tail resolve a claude dispatch by id and read its events.
func TestAgentsJobLifecycleClaude(t *testing.T) {
	cfgDir(t)
	env := wire.NewEnvelope(wire.Result{Agent: "claude", Answer: "from-event", SessionID: "sess-1"}, nil, "")
	rec := seedClaudeJob(t)
	env.DispatchID = rec.ID
	writeEvents(t, rec, []wire.Event{
		{Schema: 1, Seq: 0, Type: wire.EventStart},
		{Schema: 1, Seq: 1, Type: wire.EventText, Text: "hi"},
		{Schema: 1, Seq: 2, Type: wire.EventResult, Envelope: &env},
	})

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("status", []string{rec.ID}) })
	if code != 0 || !strings.Contains(out, rec.ID) || !strings.Contains(out, "ok") {
		t.Errorf("status = %d %q, want 0 with id and ok", code, out)
	}
	if got, _ := jobResultJSON(t, rec.ID); got.Answer != "from-event" || got.DispatchID != rec.ID {
		t.Errorf("envelope = %+v, want the result event's", got)
	}
	out = captureStdout(t, func() { code = cmdAgentsJob("tail", []string{rec.ID, "--format", "json", "-f"}) })
	checkJobTailEvents(t, code, out)
}

// An old record with no events file rebuilds its envelope from record + log.
func TestAgentsResultJSONWithoutEvents(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	got, _ := jobResultJSON(t, rec.ID)
	if got.Answer != "hi there\n" || got.SessionID != "sess-1" || got.DispatchID != rec.ID || got.Agent != "claude" {
		t.Errorf("rebuilt envelope = %+v, want log answer, session and id from the record", got)
	}
}

// An oversized event line warns on stderr and stdout stays a valid envelope.
func TestAgentsResultWarnsOnUnreadableEvents(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	if err := os.WriteFile(rec.EventsPath(), []byte(strings.Repeat("x", 17<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, stderr := jobResultJSON(t, rec.ID)
	if got.Answer != "hi there\n" {
		t.Errorf("answer = %q, want the log rebuilt into the envelope", got.Answer)
	}
	if !strings.Contains(stderr, rec.ID) || !strings.Contains(stderr, "token too long") {
		t.Errorf("stderr = %q, want a warning naming the dispatch and the scan error", stderr)
	}
}

// labelConfig is a config dir whose agents/ defines Plan, so the dispatch type gate allows it.
func labelConfig(t *testing.T) {
	t.Helper()
	hosttest.WriteRel(t, cfgDir(t), "agents/Plan.md", "---\nname: Plan\n---\n")
}

func TestAgentsLabelPrintsPrefix(t *testing.T) {
	labelConfig(t)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdAgentsLabel([]string{"Plan", "--session-model", "databricks-claude-opus-5-5"})
	})
	if code != 0 || out != "databricks-claude-opus-5-5: \n" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want the session prefix only", code, out, stderr)
	}
}

func TestAgentsLabelEmptyMeansNoPrefix(t *testing.T) {
	labelConfig(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Chdir(t.TempDir())
	for _, typ := range []string{"Explore", "Plan"} {
		for _, format := range []string{"text", "json"} {
			t.Run(typ+"/"+format, func(t *testing.T) {
				code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsLabel([]string{typ, "--format", format}) })
				if code != 0 || stderr != "" {
					t.Fatalf("exit = %d, stderr = %q, want success without diagnostics", code, stderr)
				}
				if format == "text" {
					if out != "" {
						t.Fatalf("stdout = %q, want no prefix", out)
					}
					return
				}
				var row map[string]json.RawMessage
				if err := json.Unmarshal([]byte(out), &row); err != nil {
					t.Fatal(err)
				}
				if got := string(row["label"]); got != `""` {
					t.Fatalf("JSON label = %s, want an empty string", got)
				}
			})
		}
	}
}

// A type the hook refuses prints its block reason and fails, never a prefix the hook would not accept.
func TestAgentsLabelRefusedType(t *testing.T) {
	labelConfig(t)
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsLabel([]string{"web-fetch", "--session-model", "s"}) })
	if code != 1 || out != "" || !strings.Contains(stderr, `subagent_type "web-fetch" is not on the roster`) {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want exit 1 and the block reason", code, out, stderr)
	}
}

func TestAgentsLabelStdoutFailure(t *testing.T) {
	labelConfig(t)
	hosttest.ClosedStdout(t)
	if code := cmdAgentsLabel([]string{"Plan", "--session-model", "databricks-claude-opus-5-5"}); code != 1 {
		t.Fatalf("exit = %d, want 1 when writing the prefix fails", code)
	}
}

// requireLostOutput reruns run with a closed stdout and wants exit 1, diagnostic and the write error on stderr.
func requireLostOutput(t *testing.T, run func() int, diagnostic string) {
	t.Helper()
	full := closedFile(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	if code != 1 || !strings.Contains(stderr, diagnostic) || !strings.Contains(stderr, os.ErrClosed.Error()) {
		t.Fatalf("lost output: exit %d, stderr %q; want exit 1 with %q and write error", code, stderr, diagnostic)
	}
}

func TestAgentsResultReportsTextWriteFailure(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	run := func() int { return cmdAgentsJob("result", []string{rec.ID}) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "hi there") {
		t.Fatalf("result control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireLostOutput(t, run, "agents result: write output:")
}

// A failed dispatch makes `agents wait` print one row per id, in order, and exit 1.
func TestAgentsWaitExitsOneWhenAnyDispatchErrored(t *testing.T) {
	cfgDir(t)
	okRec, errRec := seedClaudeJob(t), seedClaudeJob(t)
	errRec.ID, errRec.Status = errRec.ID+"-e", "error"
	if err := errRec.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runAgents(t, "wait", okRec.ID, errRec.ID)
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if code != 1 || len(rows) != 2 || !strings.HasPrefix(rows[0], okRec.ID+" done ") || !strings.HasPrefix(rows[1], errRec.ID+" error ") {
		t.Fatalf("wait = %d %q (stderr %q), want two rows done/error and exit 1", code, out, stderr)
	}
	if code, _, _ := runAgents(t, "wait"); code != 2 {
		t.Errorf("wait without ids = %d, want 2", code)
	}
}

// `agents wait` on a dispatch still running at --timeout exits 4, not the 1 of a failed dispatch.
func TestAgentsWaitTimeoutExitsFour(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	rec.Status, rec.PID, rec.Time = "running", os.Getpid(), time.Now()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runAgents(t, "wait", rec.ID, "--timeout", "50ms")
	if code != 4 || !strings.HasPrefix(out, rec.ID+" timeout ") {
		t.Fatalf("wait = %d %q (stderr %q), want a timeout row and exit 4", code, out, stderr)
	}
}

// `agents result --verdict` prints a status line and only the last --lines non-empty answer lines.
func TestAgentsResultVerdictPrintsStatusAndTail(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.Log, []byte("a\n\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := okAgents(t, "result", rec.ID, "--verdict", "--lines", "2")
	if want := rec.ID + " claude ok 42ms\nc\nd\n"; out != want {
		t.Fatalf("verdict = %q, want %q", out, want)
	}
	var env struct {
		DispatchID string   `json:"dispatch_id"`
		Verdict    []string `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(okAgents(t, "result", rec.ID, "--verdict", "--lines", "2", "--format", "json")), &env); err != nil ||
		env.DispatchID != rec.ID || strings.Join(env.Verdict, ",") != "c,d" {
		t.Fatalf("verdict json = %+v, %v; want the envelope id and verdict [c d]", env, err)
	}
	if code, _, _ := runAgents(t, "result", rec.ID, "--verdict", "--save", filepath.Join(t.TempDir(), "x")); code != 2 {
		t.Errorf("--verdict with --save = %d, want 2", code)
	}
}

// `agents result --save F` writes exactly what plain `agents result` prints and prints one summary line.
func TestAgentsResultSaveMatchesPlainResult(t *testing.T) {
	cfgDir(t)
	rec := seedClaudeJob(t)
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	plain := okAgents(t, "result", rec.ID)
	dest := filepath.Join(t.TempDir(), "out.txt")
	out := okAgents(t, "result", rec.ID, "--save", dest, "--head", "1")
	saved, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("saved %d B, %d lines -> %s\nhi there\n", len(plain), strings.Count(plain, "\n"), dest)
	if string(saved) != plain || out != want {
		t.Fatalf("saved %q, stdout %q; want the plain result %q and stdout %q", saved, out, plain, want)
	}
	if code, _, _ := runAgents(t, "result", rec.ID, "--head", "1"); code != 2 {
		t.Errorf("--head without --save = %d, want 2", code)
	}
}

func TestAgentsTreeReportsJSONWriteFailure(t *testing.T) {
	cfgDir(t)
	run := func() int { return cmdAgents([]string{"tree", "--format", "json"}) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !json.Valid([]byte(stdout)) {
		t.Fatalf("tree control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireLostOutput(t, run, "agents tree:")
}

// serveSuccessfulLiveTurn answers one live request on listener with a result event; the channel yields its error.
func serveSuccessfulLiveTurn(t *testing.T, listener net.Listener) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- err
			return
		}
		var request wire.LiveRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		env := wire.NewEnvelope(wire.Result{Agent: "claude", Answer: "turn complete"}, nil, "")
		done <- json.NewEncoder(conn).Encode(wire.Event{Type: wire.EventResult, Envelope: &env})
	}()
	return done
}

// liveSendFixture registers a live claude record and a unix socket for it (short path: sun_path limit).
func liveSendFixture(t *testing.T) (agent.Record, net.Listener) {
	t.Helper()
	dir, err := os.MkdirTemp("", "send-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "live"}, time.Now())
	rec.ID, rec.Live, rec.PID = "send-test", true, os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", agent.LiveSocket(rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	return rec, listener
}

func TestLiveSendReportsJSONWriteFailure(t *testing.T) {
	rec, listener := liveSendFixture(t)
	run := func() int { return cmdAgentsSend([]string{rec.ID, "hello", "--format", "json"}) }
	done := serveSuccessfulLiveTurn(t, listener)
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(stdout, "turn complete") || !json.Valid([]byte(stdout)) {
		t.Fatalf("send control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	done = serveSuccessfulLiveTurn(t, listener)
	requireLostOutput(t, run, "agents send: write output:")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// profileCmd runs agentProfile and returns its exit code, stdout and stderr.
func profileCmd(t *testing.T, name string, args ...string) (int, string, string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return agentProfile(name, args) })
}

func TestAgentProfileAndOnOffVerbs(t *testing.T) {
	vaulttest.UseFreshKey(t)
	onOff := func(name string) (int, string, string) {
		return hosttest.CaptureOutput(t, func() int { return agentOnOff(name, false, nil) })
	}
	pin := func(name string) string { return providers.AgentRecord(name).Profile }

	// copilot off leaves its record without a profile.
	if code, out, errOut := onOff("copilot"); code != 0 || !strings.Contains(out, "copilot: off") || pin("copilot") != "" {
		t.Fatalf("copilot off: code=%d out=%q err=%q pin=%q", code, out, errOut, pin("copilot"))
	}
	// profile with no args reports the off state.
	if code, out, _ := profileCmd(t, "copilot"); code != 0 || !strings.Contains(out, "off") {
		t.Fatalf("copilot profile: code=%d out=%q", code, out)
	}
	// on is a profile: with none recorded the agent stays off, and setting one switches it on.
	if code, _, errOut := profileCmd(t, "copilot", "personal"); code != 0 || !agent.AgentActive("copilot") {
		t.Fatalf("copilot profile personal: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
	if code, _, errOut := onOff("copilot"); code != 0 || agent.AgentActive("copilot") {
		t.Fatalf("copilot off with a profile: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
	// claude has no active pin.
	if code, _, errOut := onOff("claude"); code == 0 || !strings.Contains(errOut, "always on") {
		t.Fatalf("claude off: code=%d err=%q", code, errOut)
	}
	// unknown profile is refused and leaves the pin unset.
	if code, _, _ := profileCmd(t, "codex", "nosuchprofile"); code == 0 || pin("codex") != "" {
		t.Fatalf("codex profile nosuchprofile: code=%d pin=%q", code, pin("codex"))
	}
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"openai"}, "openai"}, {[]string{"--clear"}, "chatgpt"}} {
		if code, _, errOut := profileCmd(t, "codex", tc.args...); code != 0 || pin("codex") != tc.want {
			t.Fatalf("codex profile %v: code=%d pin=%q err=%q", tc.args, code, pin("codex"), errOut)
		}
	}
}

// Every vendor's help block carries a tail row.
func TestVendorUsageNamesTail(t *testing.T) {
	for _, vendor := range agent.Names() {
		if !strings.Contains(renderHelpPage(agentPage(vendor)), "tail") {
			t.Errorf("vendorUsage(%q) has no tail row", vendor)
		}
	}
}

// The per-vendor help names the profile row and shows the NAME placeholder on the launch --profile flag.
func TestVendorHelpHarnessProfileRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	cfgDir(t)
	t.Setenv("HOME", t.TempDir())
	for _, vendor := range []string{"codex", "gemini"} {
		text := renderHelpPage(agentPage(vendor))
		if !strings.Contains(text, "show or set "+vendor+"'s profile; --clear = own login") {
			t.Errorf("%s help lacks the profile row:\n%s", vendor, text)
		}
		want := "harness profile for this launch"
		if vendor == "codex" {
			want += " (codex's own: -p NAME)"
		}
		if !strings.Contains(text, "--profile NAME") {
			t.Fatalf("%s help has no `--profile NAME` flag row:\n%s", vendor, text)
		}
		if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
			t.Errorf("%s help lacks flag usage %q:\n%s", vendor, want, text)
		}
		checkCommandRows(t, text)
		for line := range strings.SplitSeq(text, "\n") {
			if w := termWidth(line); w > 80 {
				t.Errorf("%s help line %q has %d columns, want <=80", vendor, line, w)
			}
		}
	}
}

type capabilitySchemaRow struct {
	Name, Binary string
	Caps         map[string]string
	Events       []string
}

func assertCapabilitySchemaRow(t *testing.T, r capabilitySchemaRow) {
	t.Helper()
	for _, k := range []string{"cost", "background", "live", "remote", "env", "system-prompt"} {
		if r.Caps[k] == "" {
			t.Errorf("%s: caps lacks %s", r.Name, k)
		}
	}
	if r.Binary == "" || !slices.Contains(r.Events, "start") || !slices.Contains(r.Events, "result") {
		t.Errorf("%s: binary %q events %v", r.Name, r.Binary, r.Events)
	}
}

// agents --format json carries each agent's binary, the cost/background/live/remote levels and its event kinds.
func TestAgentsJSONCapabilitySchema(t *testing.T) {
	cfgDir(t)
	out, err := json.Marshal(agent.Summaries())
	if err != nil {
		t.Fatal(err)
	}
	var rows []capabilitySchemaRow
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err, string(out))
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
		assertCapabilitySchemaRow(t, r)
	}
	if c := rows[byName["claude"]]; c.Caps["live"] != "native" || c.Caps["cost"] != "native" {
		t.Errorf("claude caps = %v, want live and cost native", c.Caps)
	}
	if g := rows[byName["gemini"]]; g.Binary != "agy" || g.Caps["live"] != "native" {
		t.Errorf("gemini = %+v, want binary agy and live native", g)
	}
}

// `agents brief` prints the Owned line the diff verifier reads back; an implementation brief without --own is a usage error.
func TestAgentsBriefRendersOwnedAndRejectsMissingOwn(t *testing.T) {
	task := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(task, []byte("Do the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := okAgents(t, "brief", "--task", task, "--own", "internal/a.go", "--own", "internal/b.go", "--report", "/tmp/r.md")
	if got := agent.ParseOwned(out); !slices.Equal(got, []string{"internal/a.go", "internal/b.go"}) {
		t.Errorf("Owned round trip = %q\n%s", got, out)
	}
	for _, want := range []string{"Do the thing.", "Report: /tmp/r.md", "implementation; edit only owned files"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if ro := okAgents(t, "brief", "--task", task, "--read-only"); !strings.Contains(ro, "read-only; do not edit files") {
		t.Errorf("read-only brief:\n%s", ro)
	}
	for _, args := range [][]string{{"brief", "--task", task}, {"brief", "--own", "a.go"}} {
		if code, _, _ := runAgents(t, args...); code != 2 {
			t.Errorf("agents %v exit %d, want 2", args, code)
		}
	}
}
