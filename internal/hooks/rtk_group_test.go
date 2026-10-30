package hooks

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/settings"
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
func TestRtkGroupIsOwnNonGateGroup(t *testing.T) {
	g, ok := hookspec.GroupOf("rtk-rewrite")
	if !ok {
		t.Fatal(`hookspec.GroupOf("rtk-rewrite") found no group`)
	}
	if g.Name != "rtk" {
		t.Errorf("group name = %q, want rtk", g.Name)
	}
	if g.Gate {
		t.Error("rtk group must not be a gate — the safety gate is the only gate")
	}
	if g.VendorOnly {
		t.Error("rtk group must not be vendor-only — it rewrites Claude's Bash commands")
	}
	if len(g.Events) != 1 || g.Events[0].Event != hookspec.EventPreToolUse || g.Events[0].Matcher != "Bash" {
		t.Errorf("rtk group events = %+v, want exactly PreToolUse/Bash", g.Events)
	}

	want := hookspec.DispatcherCommand(hookspec.EventPreToolUse)
	own := hookspec.EventSpec{Name: "rtk-rewrite", Event: hookspec.EventPreToolUse}.Command()
	var disp *hookspec.MatcherGroup
	for event, groups := range hookspec.Events() {
		for i := range groups {
			for _, h := range groups[i].Hooks {
				if h.Command == own {
					t.Errorf("%s carries `hook PreToolUse --rtk-rewrite` as its own command; it must run inside the dispatcher", event)
				}
				if event == hookspec.EventPreToolUse && h.Command == want {
					disp = &groups[i]
				}
			}
		}
	}
	if disp == nil {
		t.Fatalf("PreToolUse has no %q dispatcher entry", want)
	}
	var bash bool
	for m := range strings.SplitSeq(disp.Matcher, "|") {
		if m == "Bash" {
			bash = true
		}
	}
	if !bash {
		t.Errorf("dispatcher matcher = %q, want it to include Bash", disp.Matcher)
	}
}

// The safety gate judges safety and nothing else: with rtk available it must NOT rewrite the
// command — rewriting is the rtk group's job, in a separate process after the gate.
func TestRtkSplitSafetyScanNeverRewrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := fakeRTK(t)
	payload := rtkBashIn("git status")

	var stdout strings.Builder
	if rc := SafetyScan(strings.NewReader(payload), &stdout, io.Discard); rc != ExitProceed {
		t.Fatalf("SafetyScan rc = %d, want ExitProceed (%d)", rc, ExitProceed)
	}
	if stdout.Len() != 0 {
		t.Fatalf("SafetyScan wrote %q to stdout — it must never emit updatedInput; rewriting belongs to the rtk group", stdout.String())
	}

	got, ok := rtkOut(t, payload)
	if !ok || got != bin+" git status" {
		t.Fatalf("RTKRewrite command = %q (emitted=%v), want %q", got, ok, bin+" git status")
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
		if rc := RunEvent(hookspec.EventPreToolUse, false, strings.NewReader(rtkBashIn(cmd)), &out, &errb); rc != ExitProceed {
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
	if rc := RunEvent(hookspec.EventPreToolUse, false, strings.NewReader(rtkBashIn("git status")), &out, &errb); rc != ExitProceed {
		t.Fatalf("RunEvent rc = %d, want ExitProceed; stderr=%q", rc, errb.String())
	}
	if strings.Contains(out.String(), "updatedInput") {
		t.Errorf("rtk group switched off but dispatcher still rewrote: %s", out.String())
	}

	if err := settings.SaveHarnessHooks(map[string]bool{"safety": false}); err == nil {
		t.Error("SaveHarnessHooks(safety:false) succeeded — a gate group must stay on")
	}
}
