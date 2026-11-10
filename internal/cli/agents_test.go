package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/wire"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	_, stdout, _ := hosttest.CaptureOutput(t, func() int { fn(); return 0 })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"reap", "-h"}) })
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

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"reap", "--dry-run"}) })
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
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"reap"}) }); code != 0 {
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

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"reap"}) })
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
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"reap"}) }); code != 0 {
		t.Fatalf("reap on empty state exit %d, stderr=%s", code, stderr)
	}
}

// --background and -bg are a superset of the default: a running dispatch from another repo stays listed.
func TestCmdAgentsTreeBackgroundKeepsRunningDispatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, "20261024T000002.000-codex", "running", os.Getpid(), 72*time.Hour)
	for _, flag := range []string{"--background", "-bg"} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"tree", flag, "--days", "1"}) })
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", flag, code, stderr)
		}
		if !strings.Contains(stdout, "000002") {
			t.Errorf("%s dropped the running dispatch:\n%s", flag, stdout)
		}
	}
}

// `agents tree --format json` reads CLAUDE_CONFIG_DIR and emits the documented {sessions:[...]} schema.
func TestCmdAgentsTreeFormatJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFixtureFile(t, filepath.Join(dir, "projects", "-Users-fake-repo", "sess0001.jsonl"),
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"hello"},"cwd":"/Users/fake/repo","gitBranch":"main"}`)

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"tree", "--days", "365", "--format", "json"}) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"tree", "--format", "xml"}) })
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"tree", "--days", "1"}) })
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
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"tree"}) })
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
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgents([]string{"tree", "--watch", "--interval", iv}) })
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

// TestVendorModelsAliasRemoved: models is no longer a subverb; it falls through to the agent launch like any bare word.
func TestVendorModelsAliasRemoved(t *testing.T) {
	if _, ok := agentSubPages["models"]; ok {
		t.Error("models must not route to cmdVendorSub")
	}
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

// `<v> -- status` launches (generic "unknown agent"), never the status sub-verb's "<v> status:" error.
func TestVendorVerbDoubleDashForcesPassthrough(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{"--", "status"}) })
	if strings.Contains(stderr, notAVendor+" status:") {
		t.Errorf("-- status reached cmdVendorStatus instead of launchVendor: %q", stderr)
	}
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("-- status stderr = %q, want agent.Vendor's \"unknown agent\" message", stderr)
	}
}

// status/result/cancel/tail reach their handlers, proven by each handler's "<v> <verb>:" error prefix.
func TestVendorVerbRoutesStatusResultCancelToCmdVendorSub(t *testing.T) {
	for _, sub := range []string{"status", "result", "cancel", "tail"} {
		_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{sub}) })
		want := notAVendor + " " + sub + ":"
		if !strings.Contains(stderr, want) {
			t.Errorf("vendorVerb(%q, [%q]) stderr = %q, want it to contain %q", notAVendor, sub, stderr, want)
		}
	}
}

// A bare word that is not "--", "ask" or a sub-verb launches with args forwarded unchanged.
func TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{"--native-flag", "x"}) })
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("bare launch stderr = %q, want agent.Vendor's unknown-vendor message", stderr)
	}
}

// TestVendorVerbHelpFlag checks that vendor help uses the shared page and exits successfully.
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

// agentVerb runs AgentVerb.
func agentVerb(name string, args []string) int { return cmdAgentVerb(name, args) }

func saveResultRecord(t *testing.T, status, errMsg string) job.Record {
	t.Helper()
	cwd, _ := os.Getwd()
	rec := job.NewRecord("codex", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.Status, rec.Error = status, errMsg
	rec.Repo = job.RepoRoot(context.Background(), cwd)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVendorResultErrorRecordExits1(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "error", "boom happened")
	for _, args := range [][]string{{rec.ID}, {"--format", "json", rec.ID}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentResult("codex", args) })
		if code != 1 || !strings.Contains(stderr, "error: boom happened") {
			t.Errorf("args %v: exit %d stderr %q, want 1 and \"error: boom happened\"", args, code, stderr)
		}
	}
}

func TestVendorResultOKRecordExits0(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "ok", "")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentResult("codex", []string{rec.ID}) })
	if code != 0 || strings.Contains(stderr, "error:") {
		t.Errorf("exit %d stderr %q, want 0 and no error line", code, stderr)
	}
}

// setup reaches its handler: the report row (agent name first) lands on stdout.
func TestVendorVerbRoutesSetupToCmdVendorSetup(t *testing.T) {
	_, stdout, _ := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{"setup"}) })
	if !strings.Contains(stdout, notAVendor+"  agent  fail") {
		t.Errorf("vendorVerb(%q, [setup]) stdout = %q, want cmdVendorSetup's report row", notAVendor, stdout)
	}
}

// `resume` with no id fails its own usage check before ask could dispatch a real run.
func TestVendorVerbRoutesResumeToCmdVendorResume(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"resume"}) })
	if !strings.Contains(stderr, "codex resume: usage:") {
		t.Errorf("vendorVerb(resume, no id) stderr = %q, want cmdVendorResume's usage message", stderr)
	}
}

// seedTailJob saves a finished codex record whose log holds body and returns its id.
func seedTailJob(t *testing.T, status, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := job.NewRecord("codex", agent.RunOptions{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Status, rec.Log = cwd, status, logPath
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// TestVendorTailPrintsFinishedLog: tail <id> on a finished ok job prints its log and exits 0.
func TestVendorTailPrintsFinishedLog(t *testing.T) {
	id := seedTailJob(t, "ok", "line one\nline two\n")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
	if code != 0 || stdout != "line one\nline two\n" {
		t.Errorf("tail ok job = exit %d stdout %q stderr %q, want exit 0 with the log verbatim", code, stdout, stderr)
	}
}

// TestVendorTailErrorJobExitsOne: a job that ended in error still prints its log but exits 1.
func TestVendorTailErrorJobExitsOne(t *testing.T) {
	id := seedTailJob(t, "error", "boom\n")
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
	if code != 1 || !strings.Contains(stdout, "boom") {
		t.Errorf("tail error job = exit %d stdout %q, want exit 1 with the log", code, stdout)
	}
}

// TestVendorTailUnknownIDFails: an id matching no job errors with the "<v> tail:" prefix, exit 1.
func TestVendorTailUnknownIDFails(t *testing.T) {
	seedTailJob(t, "ok", "x\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", "zzzzzzzz"}) })
	if code != 1 || !strings.Contains(stderr, "codex tail:") {
		t.Errorf("tail unknown id = exit %d stderr %q, want exit 1 and the \"codex tail:\" prefix", code, stderr)
	}
}

const cliFakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

func TestCmdAnthropicRefusalsDoNotEchoSecrets(t *testing.T) {
	vaulttest.UseFreshKey(t)
	cases := [][]string{
		{"--oauth=" + cliFakeToken},
		{"--x-api-key=" + cliFakeToken},
		{"--oauth", "--x-api-key"},
		{"--x-api-key", "--profile", "anthropic"},
		{"--oauth=MISSING_KEY_FOR_TEST", "--print-env"},
	}
	for _, args := range cases {
		code, out, errOut := hosttest.CaptureOutput(t, func() int { return cmdAnthropic(args) })
		if code == 0 {
			t.Errorf("%q: exit 0", args)
		}
		if strings.Contains(out+errOut, "FAKEFAKE") {
			t.Errorf("%q echoed the secret: %q", args, out+errOut)
		}
	}
}

func TestAnthropicHelpMentionsFlags(t *testing.T) {
	var b strings.Builder
	anthropicHelp(&b)
	for _, want := range []string{"--oauth", "--x-api-key", "--model"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %s:\n%s", want, b.String())
		}
	}
}

// catalog reaches cmdVendorModels, not launchVendor, with canonical diagnostics and help.
func TestVendorVerbRoutesCatalogToCmdVendorModels(t *testing.T) {
	for _, sub := range []string{"catalog"} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{sub, "unexpected"}) })
		if code != 2 || !strings.Contains(stderr, "codex catalog: unexpected argument") {
			t.Errorf("vendorVerb(%s): code=%d stderr=%q", sub, code, stderr)
		}
		code, _, stderr = hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{sub, "--help"}) })
		if code != 0 || !strings.HasPrefix(stderr, "usage: claude-code codex catalog") {
			t.Errorf("vendorVerb(%s --help): code=%d stderr=%q", sub, code, stderr)
		}
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

func TestDispatchUsageErrorsExit2(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for name, args := range map[string][]string{
		"missing task": {"explore"},
		"no args":      {},
		"unknown role": {"nonesuch", "task"},
		"13 --part":    partArgs(13, "explore", "task"),
		"bad schema":   {"--output-schema", "{not json", "fleet", "task"},
		"schema file":  {"--output-schema", "/nonexistent/schema.json", "fleet", "task"},
	} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdAgentsDispatch(args) }); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestDispatchErrorMessagesNameTheCause(t *testing.T) {
	vaulttest.UseFreshKey(t)
	_, _, errOut := hosttest.CaptureOutput(t, func() int { return cmdAgentsDispatch(partArgs(13, "explore", "task")) })
	if !strings.Contains(errOut, "13 --part") || !strings.Contains(errOut, "cap is 12") {
		t.Errorf("part cap stderr: %q", errOut)
	}
	_, _, errOut = hosttest.CaptureOutput(t, func() int { return cmdAgentsDispatch([]string{"--output-schema", "{not json", "fleet", "task"}) })
	if !strings.Contains(errOut, "--output-schema") {
		t.Errorf("bad schema stderr: %q", errOut)
	}
	_, _, errOut = hosttest.CaptureOutput(t, func() int { return cmdAgentsDispatch([]string{"nonesuch", "task"}) })
	if !strings.Contains(errOut, `unknown role "nonesuch"`) {
		t.Errorf("unknown role stderr: %q", errOut)
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

// seedClaudeJob saves a finished claude record with a log and, when events is non-nil, an events file.
func seedClaudeJob(t *testing.T, at time.Time, events []wire.Event) job.Record {
	t.Helper()
	rec := job.NewRecord("claude", agent.RunOptions{Prompt: "say hi"}, at)
	rec.Repo = t.TempDir()
	rec.Status, rec.SessionID, rec.DurationMs = "ok", "sess-1", 42
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.LogPath(), []byte("hi there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if events != nil {
		var b strings.Builder
		for _, ev := range events {
			line, _ := json.Marshal(ev)
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

// TestAgentsCancelNotRunning: cancel on a finished record exits non-zero.
func TestAgentsCancelNotRunning(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	if code := cmdAgentsJob("cancel", []string{rec.ID}); code == 0 {
		t.Error("cancel of a finished job exited 0, want an error")
	}
}

// TestAgentsJobLifecycleClaude: status/result/tail resolve a claude dispatch by id and read its events.
func TestAgentsJobLifecycleClaude(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env := wire.NewEnvelope(wire.Result{Agent: "claude", Answer: "from-event", SessionID: "sess-1"}, nil, "")
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	env.DispatchID = rec.ID
	evs := []wire.Event{
		{Schema: 1, Seq: 0, Type: wire.EventStart},
		{Schema: 1, Seq: 1, Type: wire.EventText, Text: "hi"},
		{Schema: 1, Seq: 2, Type: wire.EventResult, Envelope: &env},
	}
	var b strings.Builder
	for _, ev := range evs {
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("status", []string{rec.ID}) })
	if code != 0 || !strings.Contains(out, rec.ID) || !strings.Contains(out, "ok") {
		t.Errorf("status = %d %q, want 0 with id and ok", code, out)
	}

	out = captureStdout(t, func() { code = cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got wire.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, out, err)
	}
	if got.Answer != "from-event" || got.DispatchID != rec.ID {
		t.Errorf("envelope = %+v, want the result event's", got)
	}

	out = captureStdout(t, func() { code = cmdAgentsJob("tail", []string{rec.ID, "--format", "json", "-f"}) })
	checkJobTailEvents(t, code, out)
}

// TestAgentsResultJSONWithoutEvents: an old record with no events file rebuilds its envelope from record + log.
func TestAgentsResultJSONWithoutEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got wire.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, out, err)
	}
	if got.Answer != "hi there\n" || got.SessionID != "sess-1" || got.DispatchID != rec.ID || got.Agent != "claude" {
		t.Errorf("rebuilt envelope = %+v, want log answer, session and id from the record", got)
	}
}

// TestAgentsResultWarnsOnUnreadableEvents: an oversized event line warns on stderr and stdout stays a valid envelope.
func TestAgentsResultWarnsOnUnreadableEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	if err := os.WriteFile(rec.EventsPath(), []byte(strings.Repeat("x", 17<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got wire.Envelope
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, stdout, err)
	}
	if got.Answer != "hi there\n" {
		t.Errorf("answer = %q, want the log rebuilt into the envelope", got.Answer)
	}
	if !strings.Contains(stderr, rec.ID) || !strings.Contains(stderr, "token too long") {
		t.Errorf("stderr = %q, want a warning naming the dispatch and the scan error", stderr)
	}
}

func TestAgentsLabelPrintsPrefix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdAgentsLabel([]string{"Plan", "--session-model", "databricks-claude-opus-5-5"})
	})
	if code != 0 || out != "databricks-claude-opus-5-5: \n" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want the session prefix only", code, out, stderr)
	}
}

func TestAgentsLabelEmptyMeansNoPrefix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Chdir(t.TempDir())
	brief := filepath.Join(t.TempDir(), "brief")
	if err := os.WriteFile(brief, []byte("Mode: compare\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"compare", []string{"Explore", "--brief", brief, "--session-model", "sess-x"}},
		{"unknown-explore-session", []string{"Explore"}},
		{"unknown-plan-session", []string{"Plan"}},
	} {
		for _, format := range []string{"text", "json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				args := append(append([]string{}, tc.args...), "--format", format)
				code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsLabel(args) })
				if code != 0 || stderr != "" {
					t.Fatalf("exit = %d, stderr = %q, want success without diagnostics", code, stderr)
				}
				assertEmptyLabelOutput(t, format, out)
			})
		}
	}
}

func assertEmptyLabelOutput(t *testing.T, format, out string) {
	t.Helper()
	if format == "json" {
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatal(err)
		}
		if got := string(row["label"]); got != `""` {
			t.Fatalf("JSON label = %s, want an empty string", got)
		}
		return
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no prefix", out)
	}
}

func TestAgentsLabelStdoutFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hosttest.ClosedStdout(t)
	if code := cmdAgentsLabel([]string{"Plan", "--session-model", "databricks-claude-opus-5-5"}); code != 1 {
		t.Fatalf("exit = %d, want 1 when writing the prefix fails", code)
	}
}

func captureFailedStdout(t *testing.T, run func() int) (int, string) {
	t.Helper()
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("requires /dev/full")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	}()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	return code, stderr
}

func requireOutputFailure(t *testing.T, code int, stderr, diagnostic string) {
	t.Helper()
	if code != 1 || !strings.Contains(stderr, diagnostic) || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("lost output: exit %d, stderr %q; want exit 1 with %q and write error", code, stderr, diagnostic)
	}
}

func TestAgentsResultReportsTextWriteFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	rec.Log = rec.LogPath()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	run := func() int { return cmdAgentsJob("result", []string{rec.ID}) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "hi there") {
		t.Fatalf("result control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stderr = captureFailedStdout(t, run)
	requireOutputFailure(t, code, stderr, "agents result: write output:")
}

func TestAgentsTreeReportsJSONWriteFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	run := func() int { return cmdAgents([]string{"tree", "--format", "json"}) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !json.Valid([]byte(stdout)) {
		t.Fatalf("tree control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stderr = captureFailedStdout(t, run)
	requireOutputFailure(t, code, stderr, "agents tree:")
}

func TestAnthropicPassesSubcommandHelp(t *testing.T) {
	vaulttest.UseFreshKey(t)
	pathstest.FakeVendor(t, "claude", `printf 'vendor argv: %s %s\n' "$1" "$2"`)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdAnthropic([]string{"mcp", "--help"})
	})
	if code != 0 || !strings.Contains(stdout, "vendor argv: mcp --help") {
		t.Fatalf("subcommand help: exit %d, stdout %q, stderr %q; want vendor MCP help", code, stdout, stderr)
	}
	code, stdout, stderr = hosttest.CaptureOutput(t, func() int { return cmdAnthropic([]string{"--help"}) })
	if code != 0 || !strings.Contains(stdout, "--oauth") || strings.Contains(stdout, "vendor argv:") {
		t.Fatalf("wrapper help control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

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

func liveSendFixture(t *testing.T) (job.Record, net.Listener) {
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
	rec := job.NewRecord("claude", agent.RunOptions{Prompt: "live"}, time.Now())
	rec.ID, rec.Live, rec.PID = "send-test", true, os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", job.LiveSocket(rec.ID))
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
	code, stderr = captureFailedStdout(t, run)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireOutputFailure(t, code, stderr, "agents send: write output:")
}

func TestAgentProfileAndOnOffVerbs(t *testing.T) {
	vaulttest.UseFreshKey(t)

	// copilot off leaves its record without a profile.
	code, out, errOut := hosttest.CaptureOutput(t, func() int { return agentOnOff("copilot", false, nil) })
	if code != 0 || !strings.Contains(out, "copilot: off") {
		t.Fatalf("copilot off: code=%d out=%q err=%q", code, out, errOut)
	}
	if got := providers.AgentRecord("copilot").Profile; got != "" {
		t.Fatalf("copilot profile = %q, want none", got)
	}

	// profile with no args reports the off state.
	code, out, _ = hosttest.CaptureOutput(t, func() int { return agentProfile("copilot", nil) })
	if code != 0 || !strings.Contains(out, "off") {
		t.Fatalf("copilot profile: code=%d out=%q", code, out)
	}

	checkCopilotProfileActivation(t)
	checkClaudeAndCodexProfiles(t)
}

func checkCopilotProfileActivation(t *testing.T) {
	t.Helper()
	// on is a profile: with none recorded the agent stays off, and setting one switches it on.
	code, _, errOut := hosttest.CaptureOutput(t, func() int { return agentProfile("copilot", []string{"personal"}) })
	if code != 0 || !agent.AgentActive("copilot") {
		t.Fatalf("copilot profile personal: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
	code, _, errOut = hosttest.CaptureOutput(t, func() int { return agentOnOff("copilot", false, nil) })
	if code != 0 || agent.AgentActive("copilot") {
		t.Fatalf("copilot off with a profile: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
}

func checkClaudeAndCodexProfiles(t *testing.T) {
	t.Helper()
	// claude has no active pin.
	code, _, errOut := hosttest.CaptureOutput(t, func() int { return agentOnOff("claude", false, nil) })
	if code == 0 || !strings.Contains(errOut, "always on") {
		t.Fatalf("claude off: code=%d err=%q", code, errOut)
	}

	// unknown profile is refused and leaves the pin unset.
	code, _, _ = hosttest.CaptureOutput(t, func() int { return agentProfile("codex", []string{"nosuchprofile"}) })
	if code == 0 || providers.AgentRecord("codex").Profile != "" {
		t.Fatalf("codex profile nosuchprofile: code=%d pin=%q", code, providers.AgentRecord("codex").Profile)
	}

	code, _, errOut = hosttest.CaptureOutput(t, func() int { return agentProfile("codex", []string{"openai"}) })
	if code != 0 || providers.AgentRecord("codex").Profile != "openai" {
		t.Fatalf("codex profile openai: code=%d pin=%q err=%q", code, providers.AgentRecord("codex").Profile, errOut)
	}

	code, _, errOut = hosttest.CaptureOutput(t, func() int { return agentProfile("codex", []string{"--clear"}) })
	if code != 0 || providers.AgentRecord("codex").Profile != "chatgpt" {
		t.Fatalf("codex profile --clear: code=%d pin=%q err=%q", code, providers.AgentRecord("codex").Profile, errOut)
	}
}

// TestVendorUsageFits80: every rendered line stays within an 80-column terminal.
func TestVendorUsageFits80(t *testing.T) {
	for _, vendor := range vendorNames() {
		usage := renderHelpPage(agentPage(vendor))
		for line := range strings.SplitSeq(strings.TrimRight(usage, "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > 80 {
				t.Errorf("vendorUsage(%q) line %q is %d runes, want <= 80", vendor, line, n)
			}
		}
	}
}

// TestVendorUsageNamesTail: every vendor's help block carries a tail row.
func TestVendorUsageNamesTail(t *testing.T) {
	for _, vendor := range vendorNames() {
		if !strings.Contains(renderHelpPage(agentPage(vendor)), "tail") {
			t.Errorf("vendorUsage(%q) has no tail row", vendor)
		}
	}
}

// The per-vendor help names the harness profile pin and shows the NAME placeholder on the launch --profile flag.
func TestVendorHelpHarnessProfileRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, vendor := range []string{"codex", "gemini"} {
		text := renderHelpPage(agentPage(vendor))
		if !strings.Contains(text, "show or set "+vendor+"'s harness profile pin") {
			t.Errorf("%s help lacks the harness profile pin row:\n%s", vendor, text)
		}
		want := "harness profile for this launch"
		if vendor == "codex" {
			want += " (codex's own: -p NAME)"
		}
		var row string
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(line, "--profile NAME") {
				row = line
			}
		}
		if row == "" {
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
