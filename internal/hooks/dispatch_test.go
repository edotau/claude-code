package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hooks/agents"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/memhook/memhooktest"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
)

func bound(group, matcher string, budget time.Duration, run core.Runner) Bound {
	return Bound{Group: group, Spec: settings.EventSpec{Name: group, Event: "PreToolUse", Matcher: matcher, Budget: budget}, Run: run}
}

func bashInput() *core.Input {
	return core.ParseInput(strings.NewReader(`{"tool_name":"Bash","hook_event_name":"PreToolUse"}`))
}

func TestDispatchMergeAndIsolation(t *testing.T) {
	var order []string
	rec := func(n string, r core.Result) core.Runner {
		return func(*core.Input) core.Result { order = append(order, n); return r }
	}
	bs := []Bound{
		bound("a", "Bash", time.Second, rec("a", core.Result{Decision: core.Ask, Reason: "why", Context: "ctxA", SystemMessage: "sa"})),
		bound("boom", "Bash", time.Second, func(*core.Input) core.Result { panic("x") }),
		bound("slow", "Bash", 20*time.Millisecond, func(*core.Input) core.Result {
			time.Sleep(300 * time.Millisecond)
			return core.Result{Decision: core.Block}
		}),
		bound("c", "Bash", time.Second, rec("c", core.Result{Decision: core.Allow, Context: "ctxC", SystemMessage: "sc"})),
		bound("skipped", "Write", time.Second, rec("skipped", core.Result{Decision: core.Block})),
	}
	var log bytes.Buffer
	res, err := Dispatch(bs, "PreToolUse", bashInput(), &log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "a,c" {
		t.Errorf("order or matcher filtering: %v", order)
	}
	if res.Decision != core.Ask || res.Reason != "why" {
		t.Errorf("strictest wins: %+v", res)
	}
	if res.Context != "ctxA\n\nctxC" || res.SystemMessage != "sa; sc" {
		t.Errorf("join: %q / %q", res.Context, res.SystemMessage)
	}
	for _, g := range []string{"boom", "slow"} {
		if !strings.Contains(log.String(), g) {
			t.Errorf("%s not logged: %q", g, log.String())
		}
	}
}

func TestDispatchBlockBeatsAsk(t *testing.T) {
	bs := []Bound{
		bound("ask", "", time.Second, func(*core.Input) core.Result { return core.Result{Decision: core.Ask} }),
		bound("block", "", time.Second, func(*core.Input) core.Result { return core.Result{Decision: core.Block, Reason: "no"} }),
		bound("allow", "", time.Second, func(*core.Input) core.Result { return core.Result{} }),
	}
	res, _ := Dispatch(bs, "PreToolUse", bashInput(), &bytes.Buffer{})
	if res.Decision != core.Block || res.Reason != "no" {
		t.Errorf("got %+v", res)
	}
}

func TestDispatchSecondUpdatedInputAndRawStdout(t *testing.T) {
	up := func(v int) core.Runner {
		return func(*core.Input) core.Result { return core.Result{UpdatedInput: map[string]any{"k": v}} }
	}
	res, err := Dispatch([]Bound{bound("u1", "", time.Second, up(1)), bound("u2", "", time.Second, up(2))}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if err == nil || res.UpdatedInput["k"] != 1 {
		t.Errorf("second updatedInput must error and be dropped: %v %+v", err, res)
	}
	raw := func(*core.Input) core.Result { return core.Result{Stdout: "RAW"} }
	res, err = Dispatch([]Bound{bound("other", "", time.Second, raw)}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if err == nil || res.Stdout != "" {
		t.Errorf("non-worktree raw stdout must error and drop: %v %q", err, res.Stdout)
	}
	res, err = Dispatch([]Bound{bound("worktree", "", time.Second, raw)}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if err != nil || res.Stdout != "RAW" {
		t.Errorf("worktree may write raw stdout: %v %q", err, res.Stdout)
	}
}

// Every declared module resolves as `hook <Event> --<module>`.
func TestContractModuleFlagsResolve(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, s := range settings.Specs() {
		var out, errw bytes.Buffer
		if code := RunArgs(s.Event, []string{"--" + s.Name}, strings.NewReader(`{}`), &out, &errw); code == 1 && strings.Contains(errw.String(), "no module") {
			t.Errorf("%s: no module on %s", s.Name, s.Event)
		}
	}
}

func bindStub(group, name string, budget time.Duration, run core.Runner) Bound {
	return Bound{Group: group, Spec: settings.EventSpec{Name: name, Event: "UserPromptSubmit", Budget: budget}, Run: run}
}

func TestDispatchCapsMergedContextLaterBlockFirst(t *testing.T) {
	a := strings.Repeat("a", 6000)
	b := strings.Repeat("b", 6000)
	bound := []Bound{
		bindStub("g1", "h1", time.Second, func(*core.Input) core.Result { return core.Result{Context: a} }),
		bindStub("g2", "h2", time.Second, func(*core.Input) core.Result { return core.Result{Context: b} }),
	}
	res, _ := Dispatch(bound, "UserPromptSubmit", &core.Input{}, io.Discard)
	if n := len([]rune(res.Context)); n > 10000 || !strings.HasPrefix(res.Context, a) || !strings.HasSuffix(res.Context, "…truncated") {
		t.Fatalf("len=%d; want <=10000, first block intact, truncated marker", n)
	}
}

func TestDispatchWaitsForOverrunHandler(t *testing.T) {
	var finished atomic.Bool
	bound := []Bound{bindStub("g1", "slow", 200*time.Millisecond, func(*core.Input) core.Result {
		time.Sleep(250 * time.Millisecond) // past the budget, well inside drain's extra period
		finished.Store(true)
		return core.Result{}
	})}
	Dispatch(bound, "UserPromptSubmit", &core.Input{}, io.Discard)
	if !finished.Load() {
		t.Fatal("Dispatch returned while an overrun handler was still running")
	}
}

func bashPayload(cmd string) *strings.Reader {
	b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": cmd}})
	return strings.NewReader(string(b))
}

// Every hookspec module has a native handler and every native handler is declared in hookspec.
func TestRegistryMatchesHookspec(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range settings.Specs() {
		declared[s.Name] = true
		if native[s.Name] == nil {
			t.Errorf("hookspec %q has no handler", s.Name)
		}
	}
	for name := range native {
		if !declared[name] {
			t.Errorf("native handler %q has no hookspec entry", name)
		}
	}
}

// bindingProbe sets up a payload only its hook reacts to and returns it with the check that the hook did react.
type bindingProbe func(t *testing.T, cfg string) (payload string, reacted func(core.Result) bool)

// writeProbe is a Write payload for path, run from cwd.
func writeProbe(cwd, path, content string) string {
	b, _ := json.Marshal(map[string]any{"tool_name": "Write", "cwd": cwd,
		"tool_input": map[string]string{"file_path": path, "content": content}})
	return string(b)
}

// guardBindingProbes are the PreToolUse guards: each blocks or rewrites only what it owns.
func guardBindingProbes() map[string]bindingProbe {
	return map[string]bindingProbe{
		"safety-scan": func(*testing.T, string) (string, func(core.Result) bool) {
			return rtkBashIn("rm -rf /"), func(r core.Result) bool { return r.Decision == core.Block }
		},
		"rtk-rewrite": func(t *testing.T, _ string) (string, func(core.Result) bool) {
			bin := fakeRTK(t)
			return rtkBashIn("git status"), func(r core.Result) bool { return r.UpdatedInput["command"] == bin+" git status" }
		},
		"permission-gate": func(t *testing.T, _ string) (string, func(core.Result) bool) {
			t.Setenv("HARNESS_HOOK_AGENT", "gemini")
			t.Setenv("HARNESS_DISPATCH_PERMISSIONS", `{"deny":[{"Tool":"Bash","Arg":"ls"}]}`)
			return `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}}}`,
				func(r core.Result) bool { return r.Decision == core.Block }
		},
		"docs-placement": func(t *testing.T, _ string) (string, func(core.Result) bool) {
			repo := hosttest.Repo(t)
			return writeProbe(repo, filepath.Join(repo, "NOTES.md"), "# n\n"),
				func(r core.Result) bool {
					return r.Decision == core.Block && strings.Contains(r.Reason, "docs-placement")
				}
		},
		"docs-plan-gate": func(_ *testing.T, cfg string) (string, func(core.Result) bool) {
			return writeProbe(cfg, filepath.Join(cfg, "plans", "draft.md"), "freeform"),
				func(r core.Result) bool { return strings.HasPrefix(r.Reason, "plan-template: BLOCKED") }
		},
		"dispatch-effort": func(*testing.T, string) (string, func(core.Result) bool) {
			return `{"tool_name":"Workflow","tool_input":{"script":"agent('x', { label: 'l' });\n"}}`, func(r core.Result) bool {
				s, _ := r.UpdatedInput["script"].(string)
				return strings.Contains(s, "model: 'sonnet'")
			}
		},
	}
}

// planBindingProbes are the plan-file hooks: each leaves its own trace (a dated copy, a save error, the skeleton).
func planBindingProbes() map[string]bindingProbe {
	skeleton := func(payload string) bindingProbe {
		return func(*testing.T, string) (string, func(core.Result) bool) {
			return payload, func(r core.Result) bool {
				return strings.HasPrefix(r.Context, "Plan format") || strings.Contains(r.Stderr, "plan-skeleton:")
			}
		}
	}
	return map[string]bindingProbe{
		"docs-plan-mirror": func(t *testing.T, cfg string) (string, func(core.Result) bool) {
			draft := filepath.Join(cfg, "plans", "draft.md")
			if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil || os.WriteFile(draft, []byte("# Plan: probe\n"), 0o600) != nil {
				t.Fatal("seed draft:", err)
			}
			return writeProbe(cfg, draft, ""), func(core.Result) bool {
				dated, _ := filepath.Glob(filepath.Join(cfg, "plans", time.Now().Format("2006-01-02")+"-*.md"))
				return len(dated) == 1
			}
		},
		"docs-plan-save": func(_ *testing.T, cfg string) (string, func(core.Result) bool) {
			return `{"tool_name":"ExitPlanMode","tool_response":{"filePath":"` + filepath.Join(cfg, "absent.md") + `"}}`,
				func(r core.Result) bool { return strings.Contains(r.Stderr, "save-plan:") }
		},
		"docs-plan-skeleton":        skeleton(`{"hook_event_name":"PreToolUse","tool_name":"EnterPlanMode","session_id":"s"}`),
		"docs-plan-skeleton-prompt": skeleton(`{"hook_event_name":"UserPromptSubmit","permission_mode":"plan","session_id":"s"}`),
	}
}

// contextBindingProbes are the hooks whose reaction is context or an exit code naming them.
func contextBindingProbes() map[string]bindingProbe {
	return map[string]bindingProbe{
		"paths-resolve": func(t *testing.T, _ string) (string, func(core.Result) bool) {
			f := filepath.Join(t.TempDir(), "notes.md")
			if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return `{"prompt":"read ` + f + `"}`, func(r core.Result) bool { return strings.Contains(r.Context, f) }
		},
		"review-simplicity": func(t *testing.T, _ string) (string, func(core.Result) bool) {
			p := filepath.Join(t.TempDir(), "a.go")
			if err := os.WriteFile(p, []byte("package fx\n\n// one\n// two\nfunc Z() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return writeProbe(filepath.Dir(p), p, ""), func(r core.Result) bool { return strings.Contains(r.Context, "long_comment") }
		},
		"worktree-mode": func(*testing.T, string) (string, func(core.Result) bool) {
			return `{"source":"startup"}`, func(r core.Result) bool { return strings.HasPrefix(r.Context, "Worktree: before") }
		},
		"worktree-create": func(t *testing.T, _ string) (string, func(core.Result) bool) {
			return `{"cwd":"` + t.TempDir() + `","name":"x"}`,
				func(r core.Result) bool { return r.ExitCode == 1 && strings.HasPrefix(r.Stderr, "worktree-create:") }
		},
		"worktree-remove": func(*testing.T, string) (string, func(core.Result) bool) {
			return `{}`, func(r core.Result) bool { return r.ExitCode == 1 && strings.HasPrefix(r.Stderr, "worktree-remove:") }
		},
	}
}

// pointerBound are the hooks with no cheap hermetic probe, checked by runner identity instead.
var pointerBound = map[string]core.Runner{
	"format-stop": quality.StopFormat, "codex-agent-route": agents.CodexAgentRoute, "codex-routing-start": agents.CodexRoutingStart,
	"codex-routing-result": agents.CodexRoutingResult, "codex-subagent-start": agents.CodexSubagentStart,
	"dispatch-failure": agents.Release, "dispatch-workflow": agents.Release, "dispatch-stop": agents.Stop,
	"dispatch-handback": agents.Handback, "docs-plan-tick": docs.Tick, "docs-plan-reminder": docs.Reminder,
	"worktree-busy": wthook.Busy, "worktree-claim": wthook.Claim, "worktree-claim-check": wthook.ClaimCheck,
}

// Bind routes every hook name to its own handler: a probe only it reacts to, else its runner's identity.
func TestBindRoutesEachHookToItsHandler(t *testing.T) {
	probes := guardBindingProbes()
	maps.Copy(probes, planBindingProbes())
	maps.Copy(probes, contextBindingProbes())
	for _, b := range Bind() {
		name := b.Spec.Name
		probe, probed := probes[name]
		want, pinned := pointerBound[name]
		switch {
		case probed:
			t.Run(name, func(t *testing.T) {
				payload, reacted := probe(t, flagHarness(t))
				if r := b.Run(core.ParseInput(strings.NewReader(payload))); !reacted(r) {
					t.Errorf("%s did not react to its probe: %+v", name, r)
				}
			})
		case pinned:
			if reflect.ValueOf(b.Run).Pointer() != reflect.ValueOf(want).Pointer() {
				t.Errorf("%s is not bound to its runner", name)
			}
		case !strings.HasPrefix(name, "memory-"): // memory-*: TestNativeMemoryEntriesAreMemhookRunners
			t.Errorf("%s has no binding probe or runner pin", name)
		}
	}
}

// Every command in the rendered codex/copilot hook files is `hook <Event> --<module>` for a module on that event.
func TestVendorHookFilesCallOnlyResolvableHooks(t *testing.T) {
	cmdRe := regexp.MustCompile(`claude-code hook (\S+)(?: --([a-z][a-z0-9-]*))?`)
	for name, render := range map[string]func() ([]byte, error){"codex": codex.CodexJSON, "copilot": copilot.CopilotJSON} {
		body, err := render()
		if err != nil {
			t.Fatal(err)
		}
		ms := cmdRe.FindAllStringSubmatch(string(body), -1)
		if len(ms) == 0 {
			t.Fatalf("%s: no hook commands rendered", name)
		}
		for _, m := range ms {
			if _, ok := native[m[2]]; !ok || !isEvent(m[1]) || eventOf(m[2]) != m[1] {
				t.Errorf("%s hook file calls %q, which no module on that event serves", name, m[0])
			}
		}
	}
}

// An oversized context on a module's own-process path is cut to the cap plus one ellipsis rune, blocks joined by a blank line.
func TestRunDirectCapsContext(t *testing.T) {
	orig := native["memory-start"]
	native["memory-start"] = func(*core.Input) core.Result {
		return core.Result{Context: core.JoinContext("a", "", strings.Repeat("x", 20000))}
	}
	t.Cleanup(func() { native["memory-start"] = orig })
	var out strings.Builder
	if code := runDirect("memory-start", strings.NewReader(`{"session_id":"s"}`), &out, io.Discard); code != core.ExitProceed {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		H struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil || !strings.HasPrefix(doc.H.AdditionalContext, "a\n\nxx") ||
		len([]rune(doc.H.AdditionalContext)) != core.ContextCapChars+1 {
		t.Errorf("cap: %v len=%d", err, len([]rune(doc.H.AdditionalContext)))
	}
}

// tierHarness points HOME/CLAUDE_CONFIG_DIR at a temp config tree and neutralizes an ambient vendor env.
func tierHarness(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	harness := filepath.Join(home, ".claude")
	if err := os.MkdirAll(harness, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", harness)
	t.Setenv("HARNESS_HOOK_AGENT", "")
	return harness
}

// flagHarness is a hermetic config tree and vault with rtk a no-op, so only the modules under test decide.
func flagHarness(t *testing.T) string {
	t.Helper()
	harness := tierHarness(t)
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("HARNESS_RTK_BIN", "claude-test-no-such-rtk-binary")
	t.Setenv("HARNESS_DOCS_PLACEMENT", "1")
	return harness
}

// runFlags is `claude-code hook <event> <flags...>` on payload.
func runFlags(event string, payload io.Reader, flags ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = RunArgs(event, flags, payload, &out, &errb)
	return code, out.String(), errb.String()
}

// denied is a block in either channel: exit 2, or the dispatcher's PreToolUse deny document.
func denied(code int, stdout string) bool {
	return code == core.ExitBlock || strings.Contains(stdout, `"permissionDecision":"deny"`)
}

// `--<module>` runs exactly that module: safety-scan alone denies rm -rf /, rtk-rewrite alone does not.
func TestHookFlagRunsExactModule(t *testing.T) {
	flagHarness(t)
	if code, out, errb := runFlags("PreToolUse", bashPayload("rm -rf /"), "--safety-scan"); !denied(code, out) {
		t.Errorf("--safety-scan on rm -rf / = %d %q %q, want a deny", code, out, errb)
	}
	if code, out, errb := runFlags("PreToolUse", bashPayload("rm -rf /"), "--rtk-rewrite"); code != core.ExitProceed || denied(code, out) {
		t.Errorf("--rtk-rewrite on rm -rf / = %d %q %q, want proceed (safety-scan must not run)", code, out, errb)
	}
}

// A module flag on an event that does not declare it errors; a guard-like one, or a stale bare guard name, blocks.
func TestHookFlagWrongEventErrors(t *testing.T) {
	flagHarness(t)
	code, _, errb := runFlags("Stop", strings.NewReader(`{}`), "--rtk-rewrite")
	if code != 1 || !strings.Contains(errb, `no module on Stop named "rtk-rewrite"`) {
		t.Errorf("hook Stop --rtk-rewrite = %d %q, want exit 1 naming the event", code, errb)
	}
	if code, _, _ := runFlags("Stop", strings.NewReader(`{}`), "--safety-scan"); code != core.ExitBlock {
		t.Errorf("hook Stop --safety-scan = %d, want ExitBlock (guard-like names fail closed)", code)
	}
	if code, _, _ := runFlags("docs-placement", strings.NewReader(`{}`)); code != core.ExitBlock {
		t.Errorf("stale bare `hook docs-placement` = %d, want ExitBlock", code)
	}
	if code, _, _ := runFlags("PreToolUse", strings.NewReader(`{}`), "--no-own"); code != 1 {
		t.Errorf("removed --no-own = %d, want exit 1", code)
	}
}

// `--all` runs the gate; the bare dispatcher leaves it to its own process.
func TestHookFlagAllIncludesGateBareExcludes(t *testing.T) {
	flagHarness(t)
	if code, out, errb := runFlags("PreToolUse", bashPayload("rm -rf /"), "--all"); !denied(code, out) {
		t.Errorf("hook PreToolUse --all on rm -rf / = %d %q %q, want a deny", code, out, errb)
	}
	if code, out, errb := runFlags("PreToolUse", bashPayload("rm -rf /")); denied(code, out) {
		t.Errorf("bare hook PreToolUse on rm -rf / = %d %q %q, want no deny (safety-scan is own-process)", code, out, errb)
	}
}

// docs-placement runs inside the bare PreToolUse dispatcher and its block survives as the merged deny.
func TestHookDocsPlacementBlocksThroughDispatcher(t *testing.T) {
	flagHarness(t)
	repo := hosttest.Repo(t)
	payload, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": repo,
		"tool_input": map[string]string{"file_path": filepath.Join(repo, "NOTES.md"), "content": "# notes\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, out, errb := runFlags("PreToolUse", bytes.NewReader(payload))
	if code != core.ExitProceed || !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "docs-placement: BLOCKED") {
		t.Errorf("bare hook PreToolUse on a root NOTES.md = %d %q %q, want the docs-placement deny", code, out, errb)
	}
}

// TestSafetyAskDocumentExactShape pins the Claude ask document: exactly one top-level key, three inner keys.
func TestSafetyAskDocumentExactShape(t *testing.T) {
	tierHarness(t)
	var out, errb bytes.Buffer
	if code := runDirect("safety-scan", bashPayload("sudo apt-get install jq"), &out, &errb); code != core.ExitProceed {
		t.Fatalf("exit = %d, want 0 (stderr %s)", code, errb.String())
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &top); err != nil {
		t.Fatalf("stdout is not JSON: %v (%s)", err, out.String())
	}
	if len(top) != 1 || top["hookSpecificOutput"] == nil {
		t.Fatalf("top-level keys = %v, want exactly [hookSpecificOutput]", keys(top))
	}
	event, decision, reason := askDocFields(t, top["hookSpecificOutput"])
	if event != "PreToolUse" || decision != "ask" || reason == "" {
		t.Errorf("event=%q decision=%q reason=%q, want PreToolUse/ask/non-empty", event, decision, reason)
	}
}

// askDocFields decodes hookSpecificOutput, failing unless it holds exactly the three string keys.
func askDocFields(t *testing.T, raw json.RawMessage) (event, decision, reason string) {
	t.Helper()
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil {
		t.Fatalf("hookSpecificOutput is not an object: %v", err)
	}
	if len(inner) != 3 {
		t.Fatalf("inner keys = %v, want exactly [hookEventName permissionDecision permissionDecisionReason]", keys(inner))
	}
	for k, dst := range map[string]*string{"hookEventName": &event, "permissionDecision": &decision, "permissionDecisionReason": &reason} {
		v, ok := inner[k]
		if !ok {
			t.Fatalf("inner keys = %v, missing %s", keys(inner), k)
		}
		if err := json.Unmarshal(v, dst); err != nil {
			t.Fatalf("%s is not a string: %v", k, err)
		}
	}
	return event, decision, reason
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSafetyDispatcherGateVsBare: `--all` carries the gate's ask, the bare dispatcher must not.
func TestSafetyDispatcherGateVsBare(t *testing.T) {
	harness := flagHarness(t)
	payload, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "cwd": t.TempDir(),
		"tool_input": map[string]string{"command": "git -C " + harness + " stash"},
	})
	t.Run("gate carries the ask", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runEvent("PreToolUse", true, bytes.NewReader(payload), &out, &errb); code != core.ExitProceed {
			t.Fatalf("exit = %d, want 0 (stderr %s)", code, errb.String())
		}
		var doc struct {
			Output struct {
				Decision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("stdout is not the ask document: %v (%s)", err, out.String())
		}
		if doc.Output.Decision != "ask" {
			t.Errorf("permissionDecision = %q, want ask (%s)", doc.Output.Decision, out.String())
		}
	})
	t.Run("bare dispatcher omits the gate decision", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runEvent("PreToolUse", false, bytes.NewReader(payload), &out, &errb); code != core.ExitProceed {
			t.Fatalf("exit = %d, want 0 (stderr %s)", code, errb.String())
		}
		if strings.Contains(out.String(), "permissionDecision") {
			t.Errorf("bare dispatcher must not carry the safety decision: %s", out.String())
		}
	})
}

// rtkHermetic points the config dir, its vault and HOME at throwaway temp dirs.
func rtkHermetic(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
}

// requireRTKGroupShape asserts the rtk group is a vendor-neutral non-gate group on PreToolUse/Bash.
func requireRTKGroupShape(t *testing.T) {
	t.Helper()
	g, ok := settings.GroupOf("rtk-rewrite")
	if !ok {
		t.Fatal(`settings.GroupOf("rtk-rewrite") found no group`)
	}
	if g.Name != "rtk" {
		t.Errorf("group name = %q, want rtk", g.Name)
	}
	if g.Gate {
		t.Error("rtk group must not be a gate — the safety gate is the only gate")
	}
	if g.AgentOnly {
		t.Error("rtk group must not be vendor-only — it rewrites Claude's Bash commands")
	}
	if len(g.Events) != 1 || g.Events[0].Event != settings.EventPreToolUse || g.Events[0].Matcher != "Bash" {
		t.Errorf("rtk group events = %+v, want exactly PreToolUse/Bash", g.Events)
	}
}

// rtkDispatcherGroup returns the PreToolUse dispatcher entry, flagging any event that runs rtk as its own command.
func rtkDispatcherGroup(t *testing.T, want string) *settings.MatcherGroup {
	t.Helper()
	own := settings.EventSpec{Name: "rtk-rewrite", Event: settings.EventPreToolUse}.Command()
	var disp *settings.MatcherGroup
	for event, groups := range settings.Events() {
		for i := range groups {
			for _, h := range groups[i].Hooks {
				if h.Command == own {
					t.Errorf("%s carries `hook PreToolUse --rtk-rewrite` as its own command; it must run inside the dispatcher", event)
				}
				if event == settings.EventPreToolUse && h.Command == want {
					disp = &groups[i]
				}
			}
		}
	}
	return disp
}

// The rtk hook is its own non-gate PreToolUse/Bash group, reached only through the event dispatcher.
func TestRtkGroupIsOwnNonGateGroup(t *testing.T) {
	requireRTKGroupShape(t)
	want := settings.DispatcherCommand(settings.EventPreToolUse)
	disp := rtkDispatcherGroup(t, want)
	if disp == nil {
		t.Fatalf("PreToolUse has no %q dispatcher entry", want)
	}
	if !slices.Contains(strings.Split(disp.Matcher, "|"), "Bash") {
		t.Errorf("dispatcher matcher = %q, want it to include Bash", disp.Matcher)
	}
}

// The bare PreToolUse dispatcher merges the rtk rewrite and carries no permissionDecision (the gate is own-process).
func TestRtkDispatcherBareRunsRtk(t *testing.T) {
	rtkHermetic(t)
	bin := fakeRTK(t)

	run := func(cmd string) map[string]json.RawMessage {
		t.Helper()
		var out, errb strings.Builder
		if rc := runEvent(settings.EventPreToolUse, false, strings.NewReader(rtkBashIn(cmd)), &out, &errb); rc != core.ExitProceed {
			t.Fatalf("%q: runEvent rc = %d, want ExitProceed; stderr=%q", cmd, rc, errb.String())
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &doc); err != nil {
			t.Fatalf("%q: dispatcher emitted non-JSON: %v\n%s", cmd, err, out.String())
		}
		return doc
	}

	doc := run("git status")
	var inner struct {
		UpdatedInput map[string]any `json:"updatedInput"`
	}
	raw, ok := doc["hookSpecificOutput"]
	if !ok {
		t.Fatalf("dispatcher emitted no hookSpecificOutput: %v", doc)
	}
	if err := json.Unmarshal(raw, &inner); err != nil {
		t.Fatal(err)
	}
	if got, _ := inner.UpdatedInput["command"].(string); got != bin+" git status" {
		t.Errorf("dispatcher updatedInput.command = %q, want %q", got, bin+" git status")
	}
	if strings.Contains(string(raw), "permissionDecision") {
		t.Errorf("dispatcher output carries a permissionDecision — the gate is not in this process: %s", raw)
	}
	// An ask-tier command still gets no decision here: with the bare dispatcher the gate decides in its own process.
	if raw := string(run("git push --force origin main")["hookSpecificOutput"]); strings.Contains(raw, "permissionDecision") {
		t.Errorf("ask-tier command: bare dispatcher output carries a permissionDecision: %s", raw)
	}
}

// The rtk group is an ordinary toggle: harnessHooks can switch it off, while the safety gate refuses to be.
func TestRtkGroupToggle(t *testing.T) {
	rtkHermetic(t)
	fakeRTK(t)

	if err := settings.SaveHarnessHooks(map[string]bool{"rtk": false}); err != nil {
		t.Fatalf("SaveHarnessHooks(rtk:false): %v", err)
	}
	var out, errb strings.Builder
	if rc := runEvent(settings.EventPreToolUse, false, strings.NewReader(rtkBashIn("git status")), &out, &errb); rc != core.ExitProceed {
		t.Fatalf("runEvent rc = %d, want ExitProceed; stderr=%q", rc, errb.String())
	}
	if strings.Contains(out.String(), "updatedInput") {
		t.Errorf("rtk group switched off but dispatcher still rewrote: %s", out.String())
	}

	if err := settings.SaveHarnessHooks(map[string]bool{"safety": false}); err == nil {
		t.Error("SaveHarnessHooks(safety:false) succeeded — a gate group must stay on")
	}
}

// fakeRTK installs a stub rtk (HARNESS_RTK_BIN) whose `rewrite <cmd>` prints FAKE_RTK_OUT or "rtk <cmd>".
func fakeRTK(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rtk")
	script := "#!/bin/sh\n[ \"$1\" = rewrite ] || exit 9\n" +
		"if [ -n \"$FAKE_RTK_OUT\" ]; then printf '%s' \"$FAKE_RTK_OUT\"; else printf '%s' \"rtk $2\"; fi\n" +
		"exit ${FAKE_RTK_EXIT:-0}\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shell.RTKBinaryEnv, bin)
	return bin
}

func rtkBashIn(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 120},
	})
	return string(b)
}

// wantVendorPromptDoc checks the vendor-specific shape of the paths-resolve JSON document.
func wantVendorPromptDoc(t *testing.T, vendor string, doc map[string]any) {
	t.Helper()
	switch vendor {
	case core.AgentClaude:
		h, _ := doc["hookSpecificOutput"].(map[string]any)
		sys, _ := doc["systemMessage"].(string)
		ctx, _ := h["additionalContext"].(string)
		if sys == "" || ctx == "" {
			t.Errorf("claude doc = %v, want systemMessage + hookSpecificOutput.additionalContext", doc)
		}
	case core.AgentCopilot:
		ctx, _ := doc["additionalContext"].(string)
		if ctx == "" || len(doc) != 1 {
			t.Errorf("copilot doc = %v, want only additionalContext", doc)
		}
	}
}

// A direct `hook UserPromptSubmit --paths-resolve` call and the UserPromptSubmit dispatcher emit the same document.
func TestPathResolverRunMatchesDispatcher(t *testing.T) {
	for _, vendor := range []string{core.AgentClaude, core.AgentCopilot} {
		t.Run(vendor, func(t *testing.T) {
			vaulttest.Use(t) // hermetic CLAUDE_CONFIG_DIR + vault (harness hook switches)
			t.Setenv(settings.AgentEnv, vendor)
			t.Setenv("CLAUDE_RECALL_SEMANTIC", "0") // keep the co-dispatched memory-recall cheap
			dir := t.TempDir()
			f := filepath.Join(dir, "notes.md")
			if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			payload := `{"prompt":"read ` + f + `"}`

			var directOut, directErr bytes.Buffer
			if code := RunArgs("UserPromptSubmit", []string{"--paths-resolve"}, strings.NewReader(payload), &directOut, &directErr); code != core.ExitProceed {
				t.Fatalf("Run exit = %d (stderr %s)", code, &directErr)
			}
			var eventOut, eventErr bytes.Buffer
			if code := runEvent("UserPromptSubmit", false, strings.NewReader(payload), &eventOut, &eventErr); code != core.ExitProceed {
				t.Fatalf("runEvent exit = %d (stderr %s)", code, &eventErr)
			}
			if directOut.String() != eventOut.String() {
				t.Fatalf("direct %q != dispatched %q", &directOut, &eventOut)
			}
			wantVendorPromptDoc(t, vendor, hosttest.OneDoc(t, directOut.String()))
		})
	}
}

// The review-simplicity module renders its advisory as PostToolUse JSON on stdout through the registry.
func TestReviewSimplicityModuleEmitsPostToolUse(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(p, []byte("package fx\n\n// one\n// two\nfunc Z() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"hook_event_name":"PostToolUse","tool_name":"Write","cwd":%q,"tool_input":{"file_path":%q}}`,
		filepath.Dir(p), p)
	var out bytes.Buffer
	if code := RunArgs("PostToolUse", []string{"--review-simplicity"}, strings.NewReader(payload), &out, &bytes.Buffer{}); code != core.ExitProceed {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), `"hookEventName":"PostToolUse"`) || !strings.Contains(out.String(), "long_comment") {
		t.Errorf("stdout = %q, want PostToolUse JSON naming long_comment", out.String())
	}
}

// TestMain stubs the live-process scans and detached spawns so no hook test reads the machine or spawns.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	tb := &hosttest.MainTB{}
	memhooktest.StubSpawn(tb, true)
	wthook.PeerAgents = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
