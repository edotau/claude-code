package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/safety"
)

// safetyRunners are the safety-area native entries and the safety runner each must be.
var safetyRunners = map[string]core.Runner{"safety-scan": safety.Scan, "rtk-rewrite": safety.RTKRewrite, "permission-gate": safety.PermissionGate}

// Every safety-area native entry is the intended safety runner.
func TestNativeSafetyEntriesAreSafetyRunners(t *testing.T) {
	for name, want := range safetyRunners {
		if reflect.ValueOf(native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: native entry is not the safety runner", name)
		}
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

// safetyScanDecision distinguishes a Claude ask from a silent allow at exit zero.
func safetyScanDecision(t *testing.T, payload io.Reader) (core.Decision, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runDirect("safety-scan", payload, &out, &errb)
	if code == core.ExitBlock {
		return core.Block, errb.String()
	}
	if code != core.ExitProceed {
		t.Fatalf("SafetyScan exit = %d, want 0 or 2", code)
	}
	if out.Len() > 0 {
		var doc struct {
			Output struct {
				Decision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("SafetyScan stdout is not JSON: %v (%s)", err, out.String())
		}
		if doc.Output.Decision == "ask" {
			return core.Ask, errb.String()
		}
	}
	return core.Allow, errb.String()
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
	harness := tierHarness(t)
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef") // hermetic vault (as in dispatch_test.go)
	t.Setenv("HARNESS_RTK_BIN", "claude-test-no-such-rtk-binary")     // rtk stays a no-op
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

// TestSafetyStaleGuardNameFailsClosed: a vendor hook file still calling the removed bash-guard blocks every command.
func TestSafetyStaleGuardNameFailsClosed(t *testing.T) {
	tierHarness(t)
	for _, cmd := range []string{"rm -rf /", "ls"} {
		if got := RunArgs("bash-guard", nil, bashPayload(cmd), io.Discard, io.Discard); got != core.ExitBlock {
			t.Fatalf("hook bash-guard on %q = %d, want ExitBlock", cmd, got)
		}
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
