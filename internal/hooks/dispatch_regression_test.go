package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook/memhooktest"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A gate group stays on even when the vault holds a switch-off that SaveHarnessHooks would have refused.
func TestHarnessGateGroupIgnoresSwitchOff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	err := vault.Update(func(tx *vault.Tx) error {
		return tx.Put(vault.Overlay, "harnessHooks", `{"safety":false,"plans":false}`)
	})
	if err != nil {
		t.Skipf("hermetic vault unavailable: %v", err)
	}
	on := enabledGroups()
	if !on("safety", true) {
		t.Error("gate group must ignore harnessHooks=false")
	}
	if on("docs", false) {
		t.Error("non-gate group must honor harnessHooks=false, a stored plans switch read as docs")
	}
}

// A Gate module never reads the group switches; a non-Gate module still proceeds when its group is switched off.
func TestRunModuleGateSkipsSwitchRead(t *testing.T) {
	flagHarness(t)
	reads := 0
	orig := loadHarnessHooks
	loadHarnessHooks = func() (map[string]bool, error) { reads++; return map[string]bool{"safety": false, "docs": false}, nil }
	t.Cleanup(func() { loadHarnessHooks = orig })
	if code, _, _ := runFlags("PreToolUse", bashPayload("rm -rf /"), "--safety-scan"); code != core.ExitBlock || reads != 0 {
		t.Errorf("--safety-scan: exit %d, switch reads %d; want ExitBlock and 0 reads", code, reads)
	}
	repo := hosttest.Repo(t)
	payload, _ := json.Marshal(map[string]any{"tool_name": "Write", "cwd": repo,
		"tool_input": map[string]string{"file_path": filepath.Join(repo, "NOTES.md"), "content": "# n\n"}})
	if code, _, errb := runFlags("PreToolUse", bytes.NewReader(payload), "--docs-placement"); code != core.ExitProceed || reads != 1 {
		t.Errorf("--docs-placement with docs off: exit %d, switch reads %d (%s); want proceed after 1 read", code, reads, errb)
	}
}

func TestDispatchBlockingGateFailsClosedOnPanic(t *testing.T) {
	gate := bound("gate", "Bash", time.Second, func(*core.Input) core.Result { panic("scan blew up") })
	gate.Spec.Blocks = true
	res, _ := Dispatch([]Bound{gate}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if res.Decision != core.Block || !strings.Contains(res.Reason, "gate") {
		t.Errorf("panicking Blocks:true handler must block, got %+v", res)
	}
}

// A retired name is a loud no-op, never a silent exit 1; an unknown guard name fails closed.
func TestRetiredAndUnknownGuardHookNames(t *testing.T) {
	var out, errw bytes.Buffer
	if code := RunArgs("old-bash-guard", nil, strings.NewReader(`{}`), &out, &errw); code != core.ExitBlock || errw.Len() == 0 {
		t.Errorf("unknown guard: code %d stderr %q", code, errw.String())
	}
	errw.Reset()
	if code := RunArgs("access-trace", nil, strings.NewReader(`{}`), &out, &errw); code != 1 {
		t.Errorf("a removed non-guard name exits 1, got %d", code)
	}
}

// TestSafetyScanAttributionFooterCopilotShape: the Copilot payload gets the reason as permissionDecisionReason.
func TestSafetyScanAttributionFooterCopilotShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmd := `git commit -m "feat: x" -m "Co-Authored-By: Claude <noreply@anthropic.com>"`
	payload := bytes.NewReader(copilotJSON(cmd))
	var out, errb bytes.Buffer
	if got := runDirect("safety-scan", payload, &out, &errb); got != core.ExitBlock {
		t.Fatalf("%q: got %d want ExitBlock (out=%s err=%s)", cmd, got, out.String(), errb.String())
	}
	var doc struct {
		Decision string `json:"permissionDecision"`
		Reason   string `json:"permissionDecisionReason"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Decision != "deny" || !strings.Contains(strings.ToLower(doc.Reason), "attribution") {
		t.Errorf("permissionDecisionReason missing 'attribution': %s", out.String())
	}
}

func TestCopilotPayloadOverridesCodexVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_HOOK_AGENT", "codex")
	payload := `{"sessionId":"copilot","toolName":"bash","toolArgs":{"command":"sudo apt install x"}}`
	if vendor := core.ParseInput(strings.NewReader(payload)).Agent(); vendor != core.AgentCopilot {
		t.Fatalf("Vendor() = %q, want %q", vendor, core.AgentCopilot)
	}
	var stdout strings.Builder
	if rc := runDirect("safety-scan", strings.NewReader(payload), &stdout, io.Discard); rc != core.ExitBlock {
		t.Fatalf("rc = %d, want ExitBlock (%d)", rc, core.ExitBlock)
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

// copilotJSON is the Copilot-shaped Bash hook payload for cmd.
func copilotJSON(cmd string) []byte {
	b, _ := json.Marshal(map[string]any{"sessionId": "x", "toolName": "Bash", "toolArgs": map[string]string{"command": cmd}})
	return b
}

type failedDenyWriter struct{ attempted string }

func (w *failedDenyWriter) Write(p []byte) (int, error) {
	w.attempted = string(p)
	return 0, io.ErrClosedPipe
}

func TestGeminiDenyWriteFailureBlocks(t *testing.T) {
	t.Setenv("HARNESS_HOOK_AGENT", "gemini")
	t.Setenv("HARNESS_DISPATCH_PERMISSIONS", `{"deny":[{"Tool":"Bash","Arg":"ls"}]}`)
	payload := `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}}}`
	var out failedDenyWriter
	var errb bytes.Buffer
	code := runDirect("permission-gate", strings.NewReader(payload), &out, &errb)
	if code != core.ExitBlock {
		t.Fatalf("lost Gemini deny: exit %d, want ExitBlock; stderr %q", code, errb.String())
	}
	var doc struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out.attempted), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Decision != "deny" || doc.Reason == "" {
		t.Fatalf("attempted stdout %q, want deny with reason", out.attempted)
	}
	if !strings.Contains(errb.String(), doc.Reason) || !strings.Contains(errb.String(), io.ErrClosedPipe.Error()) {
		t.Fatalf("stderr %q must include deny reason and write error", errb.String())
	}
}

// agy blocks on a JSON deny at exit 0, not exit 2: the permission-gate entry prints {"decision":"deny","reason":…}.
func TestPermissionGateAgyDenyIsJSON(t *testing.T) {
	t.Setenv("HARNESS_HOOK_AGENT", "gemini")
	t.Setenv("HARNESS_DISPATCH_PERMISSIONS", `{"deny":[{"Tool":"Bash","Arg":"rm *"}]}`)
	var out, errb bytes.Buffer
	payload := `{"toolCall":{"name":"run_command","args":{"CommandLine":"bash -c \"rm v3\""}},"workspacePaths":["/w"]}`
	if code := runDirect("permission-gate", strings.NewReader(payload), &out, &errb); code != core.ExitProceed {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, errb.String())
	}
	var doc struct{ Decision, Reason string }
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Decision != "deny" || !strings.Contains(doc.Reason, `bash -c "rm v3"`) {
		t.Fatalf("stdout %q (%v), want a deny naming the command", out.String(), err)
	}
	out.Reset()
	if code := runDirect("permission-gate", strings.NewReader(`{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}}}`), &out, &errb); code != core.ExitProceed || out.Len() != 0 {
		t.Fatalf("allowed call: exit %d stdout %q, want silent", code, out.String())
	}
}

// A worktree create or remove that panics or overruns exits 1: exit 0 would report a worktree made or removed.
func TestWorktreeDispatchFailureExitsNonzero(t *testing.T) {
	for _, tc := range []struct{ module, failure string }{
		{"worktree-create", "panic"}, {"worktree-create", "timeout"}, {"worktree-remove", "panic"}, {"worktree-remove", "timeout"},
	} {
		t.Run(tc.module+"/"+tc.failure, func(t *testing.T) {
			release, finished := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(release); <-finished })
			spec := specOf(tc.module)
			spec.Budget = time.Millisecond
			run := func(*core.Input) core.Result {
				defer close(finished)
				if tc.failure == "panic" {
					panic(tc.module + " failed")
				}
				<-release
				return core.Result{Stdout: "/unusable/worktree"}
			}
			var out, errb bytes.Buffer
			res, err := Dispatch([]Bound{{Group: "worktree", Spec: spec, Run: run}}, spec.Event, &core.Input{}, &errb)
			if err != nil {
				t.Fatal(err)
			}
			code := core.Emit(core.AgentClaude, spec.Event, res, &out, &errb)
			if code != 1 || out.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q; want exit 1 without a worktree path", code, out.String())
			}
			if !strings.Contains(errb.String(), tc.module) {
				t.Fatalf("missing failed handler diagnostic: %q", errb.String())
			}
		})
	}
}

// A declared module on the wrong event exits by its Blocks flag, and a blocking one names the event it runs on.
func TestDeclaredModuleOnWrongEventDecidesByBlocks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, s := range settings.Specs() {
		event := settings.EventStop
		if s.Event == event {
			event = settings.EventSessionEnd
		}
		want := map[bool]int{false: 1, true: core.ExitBlock}[s.Blocks]
		var out, errb bytes.Buffer
		code := RunArgs(event, []string{"--" + s.Name}, strings.NewReader(`{}`), &out, &errb)
		if code != want || s.Blocks != strings.Contains(errb.String(), "runs on "+s.Event) {
			t.Errorf("hook %s --%s: exit %d stderr %q; want exit %d, naming %s only when Blocks", event, s.Name, code, errb.String(), want, s.Event)
		}
	}
}

// A gate whose blocking call outlasts its HookContext fails open before the dispatcher's timer calls it an overrun.
func TestHookContextExpiresBeforeDispatcherBudget(t *testing.T) {
	spec := specOf("docs-plan-gate") // Blocks: an overrun would block the call
	orig := core.HookMargin
	t.Cleanup(func() { core.HookMargin = orig })
	core.HookMargin = spec.Budget - 30*time.Millisecond // HookContext reads the registry budget: its deadline is 30ms in
	spec.Budget = 80 * time.Millisecond
	run := func(*core.Input) core.Result {
		ctx, cancel := core.HookContext(spec.Name)
		defer cancel()
		<-ctx.Done()
		return core.Result{Stderr: fmt.Sprintf("failed open: %v", ctx.Err())}
	}
	var log bytes.Buffer
	res, err := Dispatch([]Bound{{Group: "docs", Spec: spec, Run: run}}, spec.Event, &core.Input{ToolName: "Write"}, &log)
	if err != nil || res.Decision == core.Block || !strings.Contains(res.Stderr, "failed open: context deadline exceeded") {
		t.Fatalf("got %+v err %v log %q; want the handler's own fail-open, no overrun", res, err, log.String())
	}
	core.HookMargin = specOf(spec.Name).Budget // a margin that eats the budget leaves half of it, never zero
	ctx, cancel := core.HookContext(spec.Name)
	defer cancel()
	if dl, _ := ctx.Deadline(); time.Until(dl) < specOf(spec.Name).Budget/4 {
		t.Fatalf("deadline %s away; want about half the %s budget", time.Until(dl), specOf(spec.Name).Budget)
	}
}

// A failing worktree handler must fail the dispatcher: Claude Code reads exit 0 as "worktree created".
func TestWorktreeFailureKeepsExitCode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := native["worktree-create"]
	native["worktree-create"] = func(*core.Input) core.Result { return core.Result{Stderr: "worktree-create: boom", ExitCode: 1} }
	t.Cleanup(func() { native["worktree-create"] = orig })

	var out, errb strings.Builder
	code := runEvent("WorktreeCreate", false, strings.NewReader(`{"hook_event_name":"WorktreeCreate","name":"x"}`), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want exit 1, empty stdout, stderr kept", code, out.String(), errb.String())
	}
}

// A background Agent's PostToolUse reaches no dispatch runner (dispatch-workflow matches Workflow only): claim stays.
func TestAgentPostToolUseKeepsBackgroundClaim(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if inflight.SessionOwner() == 0 {
		t.Skip("session owner unavailable")
	}
	pre := `{"hook_event_name":"PreToolUse","tool_name":"Agent","tool_use_id":"toolu_bg","tool_input":{"prompt":"x","model":"sonnet"}}`
	post := `{"hook_event_name":"PostToolUse","tool_name":"Agent","tool_use_id":"toolu_bg","tool_response":{"status":"async_launched"}}`
	runEvent("PreToolUse", false, strings.NewReader(pre), &bytes.Buffer{}, &bytes.Buffer{})
	runEvent("PostToolUse", false, strings.NewReader(post), &bytes.Buffer{}, &bytes.Buffer{})
	if m, _ := filepath.Glob(filepath.Join(inflight.Dir(), inflight.Prefix+"*toolu_bg")); len(m) != 1 {
		t.Fatalf("claim markers after the Agent PostToolUse = %v, want the one claimed at PreToolUse", m)
	}
}

// `claude-code hook PreToolUse --codex-agent-route` (RunArgs) must emit the same wire shape the native runner renders.
func TestCodexAgentRouteThroughRunKeepsWireShape(t *testing.T) {
	root := vaulttest.Use(t)
	t.Setenv(settings.AgentEnv, core.AgentCodex)
	t.Setenv(settings.CodexProfileEnv, "probe")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"probe":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}},"routing":{"cells":{"codex":{"sonnet":{"model":"gpt-6-astra","effort":"high"}}}}}`)
	catalog := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"models":[{"slug":"databricks-gpt-6-astra","visibility":"list"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(settings.CodexCatalogEnv, catalog)
	body, _ := json.Marshal(map[string]any{"session_id": "native-session", "tool_use_id": "spawn-1", "tool_name": "collaborationspawn_agent",
		"model": "databricks-gpt-6-1-sol", "tool_input": map[string]any{"fork_turns": "none", "message": "add a --verbose flag", "task_name": "probe_child", "agent_type": "default"}})

	var directOut, directErr, runOut, runErr bytes.Buffer
	if code := runDirect("codex-agent-route", bytes.NewReader(body), &directOut, &directErr); code != core.ExitProceed {
		t.Fatalf("direct exit = %d", code)
	}
	if code := RunArgs("PreToolUse", []string{"--codex-agent-route"}, bytes.NewReader(body), &runOut, &runErr); code != core.ExitProceed {
		t.Fatalf("Run exit = %d (stderr %s)", code, &runErr)
	}
	if runOut.Len() == 0 || runOut.String() != directOut.String() {
		t.Fatalf("Run output %q != direct %q (want a non-empty routed allow)", &runOut, &directOut)
	}
	h, _ := hosttest.OneDoc(t, runOut.String())["hookSpecificOutput"].(map[string]any)
	if updated, _ := h["updatedInput"].(map[string]any); h["permissionDecision"] != "allow" || updated["model"] != "databricks-gpt-6-astra" {
		t.Fatalf("wire shape = %v", h)
	}
}

// stopFormatFixture is a git repo holding an unformatted mine.go plus a transcript that triggers the stop gates.
type stopFormatFixture struct {
	mine  string
	ugly  string
	stop  func(id string, active bool) (int, string)
	reset func()
}

// newStopFormatFixture hermetically sets up the Stop event run through the checkpoint, update and format groups.
func newStopFormatFixture(t *testing.T) stopFormatFixture {
	t.Helper()
	for _, bin := range []string{"gofmt", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv(memory.UpdateChildEnv, "")
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
	memhooktest.StubSpawn(t, false)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ugly := "package x\nfunc  F( ) {}\n"
	mine := filepath.Join(repo, "mine.go")
	reset := func() { _ = os.WriteFile(mine, []byte(ugly), 0o644) }
	reset()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	write := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n", mine)
	usage := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":180000,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n"
	_ = os.WriteFile(tr, []byte(strings.Repeat(edit, 5)+write+usage), 0o600)
	stop := func(id string, active bool) (int, string) {
		p := fmt.Sprintf(`{"session_id":%q,"cwd":%q,"transcript_path":%q,"stop_hook_active":%t}`, id, repo, tr, active)
		var out, errb bytes.Buffer
		code := runEvent("Stop", false, strings.NewReader(p), &out, &errb)
		return code, errb.String() + out.String()
	}
	return stopFormatFixture{mine: mine, ugly: ugly, stop: stop, reset: reset}
}

// Stop merges both memory blocks, still formats, skips only the checkpoint when active, and honors format off.
func TestStopCombinesBlocksAndStillFormats(t *testing.T) {
	f := newStopFormatFixture(t)
	mine, ugly, stop, reset := f.mine, f.ugly, f.stop, f.reset
	code, msg := stop("s1", false)
	if code != core.ExitBlock {
		t.Fatalf("the memory update blocks: got %d, want ExitBlock; msg=%q", code, msg)
	}
	if !strings.Contains(msg, "memory update") {
		t.Errorf("block message missing the memory update: %q", msg)
	}
	if b, _ := os.ReadFile(mine); string(b) == ugly {
		t.Error("the format group must run even when the stop is blocked")
	}

	code, msg = stop("s2", true)
	if code != core.ExitBlock || strings.Contains(msg, "Context usage") {
		t.Errorf("stop_hook_active must skip the checkpoint only: code=%d msg=%q", code, msg)
	}

	if err := settings.SaveHarnessHooks(map[string]bool{"format": false}); err != nil {
		t.Skipf("hermetic vault unavailable: %v", err)
	}
	reset()
	if code, msg = stop("s3", false); code != core.ExitBlock || !strings.Contains(msg, "memory update") {
		t.Errorf("memory must still block with format off: code=%d msg=%q", code, msg)
	}
	if b, _ := os.ReadFile(mine); string(b) != ugly {
		t.Error("a disabled format group must leave the file unformatted")
	}
}
