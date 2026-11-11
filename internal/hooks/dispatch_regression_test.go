package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestDispatchBlockingGateFailsClosedOnPanic(t *testing.T) {
	gate := bound("gate", "Bash", time.Second, func(*core.Input) core.Result { panic("scan blew up") })
	gate.Spec.Blocks = true
	res, _ := Dispatch([]Bound{gate}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if res.Decision != core.Block || !strings.Contains(res.Reason, "gate") {
		t.Errorf("panicking Blocks:true handler must block, got %+v", res)
	}
}

// unparseable are a garbled and a truncated hook payload.
var unparseable = []string{`{not json`, `{"tool_name":"Bash","tool_input":{"command":"rm -rf /ho`}

// `hook PreToolUse --<gate>` blocks a payload that does not parse; a non-gate module still runs and proceeds.
func TestRunModuleGateBlocksUnparseablePayload(t *testing.T) {
	flagHarness(t)
	ran := 0
	orig := native["rtk-rewrite"]
	native["rtk-rewrite"] = func(in *core.Input) core.Result { ran++; return orig(in) }
	t.Cleanup(func() { native["rtk-rewrite"] = orig })
	for _, payload := range unparseable {
		code, _, errb := runFlags("PreToolUse", strings.NewReader(payload), "--safety-scan")
		if code != core.ExitBlock || !strings.Contains(errb, "safety-scan: unreadable hook payload (") {
			t.Errorf("--safety-scan on %q: exit %d stderr %q; want ExitBlock naming the hook and payload", payload, code, errb)
		}
		if code, out, errb := runFlags("PreToolUse", strings.NewReader(payload), "--rtk-rewrite"); denied(code, out) {
			t.Errorf("--rtk-rewrite on %q: exit %d stdout %q stderr %q; a non-gate must not block", payload, code, out, errb)
		}
	}
	if ran != len(unparseable) {
		t.Errorf("rtk-rewrite ran %d times, want %d", ran, len(unparseable))
	}
	if code, _, errb := runFlags("PreToolUse", bashPayload("ls -la"), "--safety-scan"); code != core.ExitProceed {
		t.Errorf("valid ls: exit %d stderr %q, want proceed", code, errb)
	}
	if code, _, errb := runFlags("PreToolUse", bashPayload("rm -rf /"), "--safety-scan"); code != core.ExitBlock || strings.Contains(errb, "unreadable") {
		t.Errorf("valid rm -rf /: exit %d stderr %q, want the scan's own block", code, errb)
	}
}

// The event form blocks an unparseable payload for every gate on the event, matcher or not; non-gates still run.
func TestDispatchGateBlocksUnparseablePayload(t *testing.T) {
	gateRan, otherRan := 0, 0
	gate := bound("gate", "Bash", time.Second, func(*core.Input) core.Result { gateRan++; return core.Result{} })
	gate.Spec.Blocks = true
	other := bound("other", "", time.Second, func(*core.Input) core.Result { otherRan++; return core.Result{Context: "ran"} })
	for _, payload := range unparseable {
		res, _ := Dispatch([]Bound{gate, other}, "PreToolUse", core.ParseInput(strings.NewReader(payload)), io.Discard)
		if res.Decision != core.Block || !strings.Contains(res.Reason, "gate: unreadable hook payload (") || res.Context != "ran" {
			t.Errorf("%q: got %+v; want a block naming the gate plus the non-gate's context", payload, res)
		}
	}
	if gateRan != 0 || otherRan != len(unparseable) {
		t.Errorf("gate ran %d, non-gate ran %d; want 0 and %d", gateRan, otherRan, len(unparseable))
	}
	if res, _ := Dispatch([]Bound{gate, other}, "PreToolUse", bashInput(), io.Discard); res.Decision != core.Allow || gateRan != 1 {
		t.Errorf("valid payload: got %+v, gate ran %d; want allow after the gate ran once", res, gateRan)
	}
}

// `hook PreToolUse` renders the unparseable-payload block as Claude's deny document; a valid payload is not denied.
func TestRunEventGateBlocksUnparseablePayload(t *testing.T) {
	flagHarness(t)
	if code, out, _ := runFlags("PreToolUse", strings.NewReader(unparseable[0])); !denied(code, out) || !strings.Contains(out, "unreadable hook payload") {
		t.Errorf("hook PreToolUse on a garbled payload: exit %d stdout %q; want a deny naming the payload", code, out)
	}
	if code, out, errb := runFlags("PreToolUse", bashPayload("ls -la")); denied(code, out) {
		t.Errorf("hook PreToolUse on valid ls: exit %d stdout %q stderr %q; want no deny", code, out, errb)
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
	mine string
	ugly string
	stop func(id string, active bool) (int, string)
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
	memory.StubSpawn(t, false)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ugly := "package x\nfunc  F( ) {}\n"
	mine := filepath.Join(repo, "mine.go")
	_ = os.WriteFile(mine, []byte(ugly), 0o644)
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
	return stopFormatFixture{mine: mine, ugly: ugly, stop: stop}
}

// Stop merges both memory blocks, still formats, and skips only the checkpoint when active.
func TestStopCombinesBlocksAndStillFormats(t *testing.T) {
	f := newStopFormatFixture(t)
	mine, ugly, stop := f.mine, f.ugly, f.stop
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
}

// hookAgents is every agent a hook payload can name.
var hookAgents = []string{core.AgentClaude, core.AgentCodex, core.AgentCopilot, core.AgentGemini, core.AgentOpencode}

// stubNatives swaps every module's Runner for one returning verdicts[name] (unset: a silent pass); ran records calls.
func stubNatives(t *testing.T, verdicts map[string]core.Result) map[string]bool {
	ran := map[string]bool{}
	for name, orig := range native {
		native[name] = func(*core.Input) core.Result { ran[name] = true; return verdicts[name] }
		t.Cleanup(func() { native[name] = orig })
	}
	return ran
}

// toolPayload is agent's hook payload naming a tool s's matcher accepts (agy's toolCall shape for gemini).
func toolPayload(agent string, s settings.EventSpec) *strings.Reader {
	tool := strings.Trim(strings.Split(s.Matcher, "|")[0], ".*$")
	if agent == core.AgentGemini {
		return strings.NewReader(fmt.Sprintf(`{"toolCall":{"name":%q,"args":{}}}`, tool))
	}
	return strings.NewReader(fmt.Sprintf(`{"hook_event_name":%q,"session_id":"s","tool_name":%q}`, s.Event, tool))
}

// nativeRuns reports whether agent's own hook file runs module name as `hook <Event> --<name>`.
func nativeRuns(agent, name string) bool {
	for _, s := range settings.SpecsFor(agent, "native") {
		if s.Name == name {
			return true
		}
	}
	return false
}

// `hook <Event> --all` runs, per agent and module, exactly what that agent's hook file runs one module at a time.
func TestEventFormAgreesWithModuleForm(t *testing.T) {
	ran := stubNatives(t, nil)
	for _, agent := range hookAgents {
		t.Setenv(settings.AgentEnv, agent)
		for _, s := range settings.Specs() {
			clear(ran)
			RunArgs(s.Event, []string{settings.AllFlag}, toolPayload(agent, s), io.Discard, io.Discard)
			if want := nativeRuns(agent, s.Name); ran[s.Name] != want {
				t.Errorf("%s · %s --%s: event form ran=%v, module form runs=%v", agent, s.Event, s.Name, ran[s.Name], want)
			}
			checkRealTools(t, ran, agent, s)
		}
	}
}

// checkRealTools runs s's event form on agent's own tool names and checks it runs s where agent's hook file would.
func checkRealTools(t *testing.T, ran map[string]bool, agent string, s settings.EventSpec) {
	for _, tool := range realTools[agent] {
		if s.Matcher == "" {
			return
		}
		clear(ran)
		RunArgs(s.Event, []string{settings.AllFlag}, realPayload(agent, s.Event, tool), io.Discard, io.Discard)
		if want := nativeRuns(agent, s.Name) && renderedMatch(t, agent, s, tool); ran[s.Name] != want {
			t.Errorf("%s · %s %s --%s: event form ran=%v, module form runs=%v", agent, s.Event, tool, s.Name, ran[s.Name], want)
		}
	}
}

// realTools are each agent's own hook tool names for a shell command and a file write.
var realTools = map[string][]string{
	core.AgentClaude:   {"Bash", "Write"},
	core.AgentCodex:    {"Bash", "apply_patch"},       // codex/permissions.go matchesBash; apply_patch is its file write
	core.AgentCopilot:  {"bash", "create"},            // copilot/home.go CopilotJSON: Write|create, lowercased names
	core.AgentGemini:   {"run_command", "write_file"}, // gemini/hooks.go agyGateMatcher
	core.AgentOpencode: {"Bash", "Write"},             // opencode/hooks.go claudeTool and plugin.go TOOLS rename to Claude's
}

// realPayload is agent's own PreToolUse/PostToolUse document for tool: agy's toolCall, Copilot's camelCase shape.
func realPayload(agent, event, tool string) *strings.Reader {
	switch agent {
	case core.AgentGemini:
		return strings.NewReader(fmt.Sprintf(`{"toolCall":{"name":%q,"args":{}}}`, tool))
	case core.AgentCopilot:
		return strings.NewReader(fmt.Sprintf(`{"sessionId":"s","toolName":%q,"toolArgs":"{}"}`, tool))
	}
	return strings.NewReader(fmt.Sprintf(`{"hook_event_name":%q,"session_id":"s","tool_name":%q}`, event, tool))
}

// renderedMatch reports whether the matcher agent's rendered hook file puts on module s selects tool.
func renderedMatch(t *testing.T, agent string, s settings.EventSpec, tool string) bool {
	m := s.Matcher
	switch agent {
	case core.AgentGemini:
		m = "run_command|read_file|write_file|edit_file" // gemini/hooks.go agyGateMatcher (unexported)
	case core.AgentCopilot:
		m = renderedMatcher(t, copilot.CopilotJSON, s.Name)
	case core.AgentCodex:
		m = renderedMatcher(t, codex.CodexJSON, s.Name)
	}
	return m == "" || regexp.MustCompile(`^(?:`+m+`)$`).MatchString(tool)
}

// renderedMatcher is the matcher on module name's entry in a vendor's rendered hooks JSON (Copilot flat, Codex nested).
func renderedMatcher(t *testing.T, render func() ([]byte, error), name string) string {
	raw, err := render()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string
			Bash    string
			Hooks   []struct{ Command string }
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, entries := range doc.Hooks {
		for _, e := range entries {
			cmds := []string{e.Bash}
			for _, h := range e.Hooks {
				cmds = append(cmds, h.Command)
			}
			for _, c := range cmds {
				if slices.Contains(strings.Fields(c), "--"+name) {
					return e.Matcher
				}
			}
		}
	}
	t.Fatalf("module %s is not in the rendered hooks", name)
	return ""
}

// consumerDeny is the stdout document agent's hook consumer refuses a call on, at exit 0, for decision d.
func consumerDeny(agent string, d core.Decision, reason string) map[string]any {
	decision := "deny"
	if d == core.Ask && (agent == core.AgentClaude || agent == core.AgentOpencode) { // Claude asks; opencode's plugin blocks on ask
		decision = "ask"
	}
	switch agent {
	case core.AgentGemini: // agy: top-level decision, exit 0
		return map[string]any{"decision": decision, "reason": reason}
	case core.AgentCopilot:
		return map[string]any{"permissionDecision": decision, "permissionDecisionReason": reason}
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": settings.EventPreToolUse, "permissionDecision": decision, "permissionDecisionReason": reason}}
}

// Any PreToolUse module's block or ask, in either form, renders so the agent's consumer refuses the call and sees why.
func TestBlockRendersAsConsumerDeny(t *testing.T) {
	verdicts := map[string]core.Result{}
	stubNatives(t, verdicts)
	for _, s := range settings.Specs() {
		if s.Event != settings.EventPreToolUse {
			continue
		}
		for _, d := range []core.Decision{core.Block, core.Ask} {
			reason := fmt.Sprintf("stub %s decision %d", s.Name, d)
			clear(verdicts)
			verdicts[s.Name] = core.Result{Decision: d, Reason: reason}
			for _, agent := range hookAgents {
				checkConsumerDeny(t, agent, s, d, reason)
			}
		}
	}
}

// checkConsumerDeny runs s in the module form, and the event form where agent binds it, and checks the refusal.
func checkConsumerDeny(t *testing.T, agent string, s settings.EventSpec, d core.Decision, reason string) {
	t.Setenv(settings.AgentEnv, agent)
	forms := [][]string{{"--" + s.Name}}
	if nativeRuns(agent, s.Name) {
		forms = append(forms, []string{settings.AllFlag})
	}
	for _, f := range forms {
		if why := consumerRefuses(agent, s, d, reason, f[0]); why != "" {
			t.Errorf("%s %s decision %d: %s", agent, f[0], d, why)
		}
	}
}

// consumerRefuses runs `hook <Event> <flag>` and is "" when agent's consumer refuses the call and gets the reason.
func consumerRefuses(agent string, s settings.EventSpec, d core.Decision, reason, flag string) string {
	code, stdout, stderr := runFlags(s.Event, toolPayload(agent, s), flag)
	if code == core.ExitBlock && agent != core.AgentGemini { // Claude, codex, copilot >= 1.0.70 and opencode's plugin
		if !strings.Contains(stderr, reason) {
			return fmt.Sprintf("exit 2 with stderr %q, want the reason", stderr)
		}
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); code != core.ExitProceed || err != nil {
		return fmt.Sprintf("exit %d stdout %q: neither exit 2 nor a deny document", code, stdout)
	}
	got, _ := json.Marshal(doc) // map keys marshal sorted, so equal documents compare equal
	if want, _ := json.Marshal(consumerDeny(agent, d, reason)); string(got) != string(want) {
		return fmt.Sprintf("stdout %s, want exactly %s", stdout, want)
	}
	return ""
}

// writePayloads are agent's PreToolUse documents writing "hi" to files: one Claude Write each, or one Codex apply_patch.
func writePayloads(agent, cwd string, files []string) []string {
	if agent == core.AgentCodex {
		var patch strings.Builder
		for _, f := range files {
			patch.WriteString("*** Add File: " + f + "\n+hi\n")
		}
		b, _ := json.Marshal(map[string]any{"hook_event_name": settings.EventPreToolUse, "session_id": "s", "cwd": cwd,
			"tool_name": "apply_patch", "tool_input": map[string]string{"command": "*** Begin Patch\n" + patch.String() + "*** End Patch\n"}})
		return []string{string(b)}
	}
	var out []string
	for _, f := range files {
		b, _ := json.Marshal(map[string]any{"hook_event_name": settings.EventPreToolUse, "session_id": "s", "cwd": cwd,
			"tool_name": "Write", "tool_input": map[string]string{"file_path": f, "content": "hi"}})
		out = append(out, string(b))
	}
	return out
}

// A Codex file write reaches the docs gates through its rendered matcher and the event form, decided as Claude's Write.
func TestCodexFileWriteRunsDocGates(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo, _ := filepath.EvalSymlinks(hosttest.Repo(t))
	for _, c := range []struct {
		gate  string
		files []string
		block bool
	}{
		{"docs-placement", []string{"NOTES.md"}, true},
		{"docs-placement", []string{"README.md"}, false},
		{"docs-placement", []string{"src/a.go", "STRAY.md"}, true},
		{"docs-plan-gate", []string{filepath.Join(cfg, "plans", "draft.md")}, true},
	} {
		if m := renderedMatcher(t, codex.CodexJSON, c.gate); !regexp.MustCompile(`^(?:` + m + `)$`).MatchString("apply_patch") {
			t.Errorf("%s: Codex's rendered matcher %q never selects apply_patch", c.gate, m)
		}
		for _, agent := range []string{core.AgentClaude, core.AgentCodex} {
			t.Setenv(settings.AgentEnv, agent)
			for _, flag := range []string{"--" + c.gate, settings.AllFlag} {
				got := false
				for _, p := range writePayloads(agent, repo, c.files) {
					code, stdout, _ := runFlags(settings.EventPreToolUse, strings.NewReader(p), flag)
					got = got || denied(code, stdout)
				}
				if got != c.block {
					t.Errorf("%s %s %s %v: denied=%v, want %v", agent, flag, c.gate, c.files, got, c.block)
				}
			}
		}
	}
}
