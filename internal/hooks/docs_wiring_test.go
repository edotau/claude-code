package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// plainDocsRunners are the docs-area Native entries registered without a viaStderr wrapper.
var plainDocsRunners = map[string]core.Runner{
	"docs-plan-skeleton": docs.Skeleton, "docs-plan-skeleton-prompt": docs.Skeleton, "docs-plan-tick": docs.Tick,
	"docs-plan-reminder": docs.Reminder, "paths-resolve": docs.ResolvePaths,
}

// stderrDocsRunners are the docs-area helpers Native wraps in viaStderr.
var stderrDocsRunners = map[string]func(*core.Input, io.Writer) int{
	"docs-placement": docs.Placement, "docs-plan-gate": docs.Gate, "docs-plan-mirror": docs.Mirror, "docs-plan-save": docs.Save,
}

// Every docs-area Native entry is the intended docs runner: same function, or the same Result as the wrapped helper.
func TestNativeDocsEntriesAreDocsRunners(t *testing.T) {
	for name, want := range plainDocsRunners {
		if reflect.ValueOf(Native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: Native entry is not the docs runner", name)
		}
	}
	for name, helper := range stderrDocsRunners {
		payload := `{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(t.TempDir(), "x.txt") + `"}}`
		var b bytes.Buffer
		want := core.ExitResult(helper(core.ParseInput(strings.NewReader(payload)), &b), b.String())
		if got := Native[name](core.ParseInput(strings.NewReader(payload))); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Native result %+v, want %+v", name, got, want)
		}
	}
}

// The paths and codex groups run natively: Bind must hand each declared handler the Runner from
// Native (same function pointer), and that Runner returns a Result directly.
func TestBindPrefersNativeRunners(t *testing.T) {
	if len(Native) == 0 {
		t.Fatal("Native is empty")
	}
	wantNativeBindings(t)
	wantPathsResolveResult(t)
}

// wantNativeBindings requires every Native runner to be bound by Bind to the same function pointer.
func wantNativeBindings(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	for _, b := range Bind() {
		native, ok := Native[b.Spec.Name]
		if !ok {
			continue
		}
		seen[b.Spec.Name] = true
		if reflect.ValueOf(b.Run).Pointer() != reflect.ValueOf(native).Pointer() {
			t.Errorf("%s: not bound to its Native runner", b.Spec.Name)
		}
	}
	for name := range Native {
		if !seen[name] {
			t.Errorf("%s: declared in Native but never bound", name)
		}
	}
}

// wantPathsResolveResult runs the bound paths-resolve Runner and requires a Result carrying Context + SystemMessage.
func wantPathsResolveResult(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := core.ParseInput(strings.NewReader(`{"prompt":"read ` + f + `"}`))
	for _, b := range Bind() {
		if b.Spec.Name != "paths-resolve" {
			continue
		}
		r := b.Run(in) // a Result straight from the Runner
		if r.Context == "" || r.SystemMessage == "" {
			t.Errorf("path-resolver Result = %+v, want Context and SystemMessage", r)
		}
	}
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
			if code := RunEvent("UserPromptSubmit", false, strings.NewReader(payload), &eventOut, &eventErr); code != core.ExitProceed {
				t.Fatalf("RunEvent exit = %d (stderr %s)", code, &eventErr)
			}
			if directOut.String() != eventOut.String() {
				t.Fatalf("direct %q != dispatched %q", &directOut, &eventOut)
			}
			wantVendorPromptDoc(t, vendor, oneDoc(t, directOut.String()))
		})
	}
}

func TestPlanTemplateCommandRetired(t *testing.T) {
	for _, old := range []string{"plan-template", "plan-gate"} {
		if _, ok := Native[old]; ok {
			t.Fatalf("retired %s module remains registered", old)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := RunArgs("PreToolUse", []string{"--plan-template"}, strings.NewReader("{}"), &stdout, &stderr); code != 1 {
		t.Fatalf("retired module returned %d, want 1", code)
	}
	if stdout.Len() != 0 || stderr.String() != "claude-code hook: no module on PreToolUse named \"plan-template\"\n" {
		t.Fatalf("stdout=%q stderr=%q", &stdout, &stderr)
	}
}

// The dispatch spawn hooks claim and release the active plan task through docs.ClaimPlanTask/ClearPlanClaim.
// planRepo makes a git repo with one dated plan holding open owned tasks 1.2 and 1.3; returns repo and plan path.
func planRepo(t *testing.T) (repo, plan string) {
	t.Helper()
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	dir := plans.Home(memstore.GitToplevel(repo))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plan = filepath.Join(dir, "2026-10-29-x.md")
	body := "# P\n\n- [ ] 1.2 wire the claim call point owns: a.go\n- [ ] 1.3 delete dead recall helper owns: b.go\n"
	if err := os.WriteFile(plan, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, plan
}

func agentPayload(repo, id, prompt, desc string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": id, "cwd": repo,
		"tool_input": map[string]any{"prompt": prompt, "description": desc, "model": "sonnet"}})
	return string(b)
}

// A Task: brief line claims that plan task on the Agent spawn and the agent's SubagentStop clears it.
func TestPlanClaimOnSpawnClearedOnStop(t *testing.T) {
	dispatchEnv(t)
	repo, plan := planRepo(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	pre(agentPayload(repo, "toolu_c1", "Objective: x\nTask: 1.2\nOwns: a.go", ""))
	data, _ := os.ReadFile(plan)
	if !regexp.MustCompile(`1\.2 wire.*<!-- working: Agent toolu_c1 · worktree main · since \d\d:\d\d -->`).Match(data) || strings.Count(string(data), "working:") != 1 {
		t.Fatalf("claim not written on 1.2 only:\n%s", data)
	}
	writeMeta(t, tx, "ac1", "toolu_c1")
	stop := `{"hook_event_name":"SubagentStop","agent_id":"ac1","cwd":"` + repo + `","transcript_path":"` + tx + `"}`
	subagentStopRunner(core.ParseInput(strings.NewReader(stop)))
	if data, _ = os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("claim survived SubagentStop:\n%s", data)
	}
}

// No Task: line and no confident description match claims nothing; a unique title match claims.
func TestPlanClaimNeedsConfidentMatch(t *testing.T) {
	dispatchEnv(t)
	repo, plan := planRepo(t)
	pre(agentPayload(repo, "toolu_n1", "no task line", "do things"))
	pre(agentPayload(repo, "toolu_n2", "Task: 9.9", ""))
	if data, _ := os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("unconfident dispatch claimed a task:\n%s", data)
	}
	pre(agentPayload(repo, "toolu_n3", "no task line", "Delete dead recall helper"))
	if data, _ := os.ReadFile(plan); !strings.Contains(string(data), "1.3 delete dead recall helper owns: b.go <!-- working: Agent toolu_n3") {
		t.Fatalf("unique description match not claimed:\n%s", data)
	}
}

// A spawn that fails never reaches SubagentStop: PostToolUseFailure must release its plan claim too.
func TestPlanClaimClearedOnSpawnFailure(t *testing.T) {
	dispatchEnv(t)
	repo, plan := planRepo(t)
	pre(agentPayload(repo, "toolu_f1", "Task: 1.2", ""))
	if data, _ := os.ReadFile(plan); !strings.Contains(string(data), "working:") {
		t.Fatalf("no claim:\n%s", data)
	}
	runEvent(t, "PostToolUseFailure", `{"hook_event_name":"PostToolUseFailure","tool_name":"Agent","tool_use_id":"toolu_f1","cwd":"`+repo+`"}`)
	if data, _ := os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("claim survived a failed spawn:\n%s", data)
	}
}
