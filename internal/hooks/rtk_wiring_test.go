package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shellscope"
)

// rtkHermetic points the config dir (and the vault it holds) at throwaway temp dirs, the way
// dispatch_test.go does; HOME moves too so nothing outside the temp dirs is touched.
func rtkHermetic(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
}

// The rtk hook lives in its own non-gate group on PreToolUse/Bash, and settings.json reaches it
// only through the event dispatcher — never as its own `hook PreToolUse --rtk-rewrite` entry.
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

// The rtk hook lives in its own non-gate group on PreToolUse/Bash, and settings.json reaches it
// only through the event dispatcher — never as its own `hook PreToolUse --rtk-rewrite` entry.
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

// The bare dispatcher (the process settings.json runs for PreToolUse) runs the rtk group and
// merges its rewrite, while the gate lives in its own process — no permissionDecision can appear.
func TestRtkDispatcherBareRunsRtk(t *testing.T) {
	rtkHermetic(t)
	bin := fakeRTK(t)

	run := func(cmd string) map[string]json.RawMessage {
		t.Helper()
		var out, errb strings.Builder
		if rc := RunEvent(settings.EventPreToolUse, false, strings.NewReader(rtkBashIn(cmd)), &out, &errb); rc != core.ExitProceed {
			t.Fatalf("%q: RunEvent rc = %d, want ExitProceed; stderr=%q", cmd, rc, errb.String())
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

// The rtk group is an ordinary toggle: harnessHooks can switch it off, while the safety gate
// refuses to be switched off.
func TestRtkGroupToggle(t *testing.T) {
	rtkHermetic(t)
	fakeRTK(t)

	if err := settings.SaveHarnessHooks(map[string]bool{"rtk": false}); err != nil {
		t.Fatalf("SaveHarnessHooks(rtk:false): %v", err)
	}
	var out, errb strings.Builder
	if rc := RunEvent(settings.EventPreToolUse, false, strings.NewReader(rtkBashIn("git status")), &out, &errb); rc != core.ExitProceed {
		t.Fatalf("RunEvent rc = %d, want ExitProceed; stderr=%q", rc, errb.String())
	}
	if strings.Contains(out.String(), "updatedInput") {
		t.Errorf("rtk group switched off but dispatcher still rewrote: %s", out.String())
	}

	if err := settings.SaveHarnessHooks(map[string]bool{"safety": false}); err == nil {
		t.Error("SaveHarnessHooks(safety:false) succeeded — a gate group must stay on")
	}
}

// fakeRTK installs a stub rtk (HARNESS_RTK_BIN) and returns its absolute path. `rewrite <cmd>` prints
// FAKE_RTK_OUT, else "rtk <cmd>", and exits FAKE_RTK_EXIT (default 0).
func fakeRTK(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rtk")
	script := "#!/bin/sh\n[ \"$1\" = rewrite ] || exit 9\n" +
		"if [ -n \"$FAKE_RTK_OUT\" ]; then printf '%s' \"$FAKE_RTK_OUT\"; else printf '%s' \"rtk $2\"; fi\n" +
		"exit ${FAKE_RTK_EXIT:-0}\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shellscope.RTKBinaryEnv, bin)
	return bin
}

func rtkBashIn(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 120},
	})
	return string(b)
}
