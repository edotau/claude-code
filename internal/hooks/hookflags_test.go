package hooks

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// flagHarness is a hermetic config tree and vault with rtk a no-op, so only the modules under test decide.
func flagHarness(t *testing.T) {
	t.Helper()
	tierHarness(t)
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("HARNESS_RTK_BIN", "claude-test-no-such-rtk-binary")
	t.Setenv(docsPlacementDisableEnv, "1")
}

// runFlags is `claude-code hook <event> <flags...>` on payload.
func runFlags(event string, payload io.Reader, flags ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = RunArgs(event, flags, payload, &out, &errb)
	return code, out.String(), errb.String()
}

// denied is a block in either channel: exit 2, or the dispatcher's PreToolUse deny document.
func denied(code int, stdout string) bool {
	return code == ExitBlock || strings.Contains(stdout, `"permissionDecision":"deny"`)
}

// `--<module>` runs exactly that module: safety-scan alone denies rm -rf /, rtk-rewrite alone does not.
func TestHookFlagRunsExactModule(t *testing.T) {
	flagHarness(t)
	if code, out, errb := runFlags("PreToolUse", bashPayload("rm -rf /"), "--safety-scan"); !denied(code, out) {
		t.Errorf("--safety-scan on rm -rf / = %d %q %q, want a deny", code, out, errb)
	}
	if code, out, errb := runFlags("PreToolUse", bashPayload("rm -rf /"), "--rtk-rewrite"); code != ExitProceed || denied(code, out) {
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
	if code, _, _ := runFlags("Stop", strings.NewReader(`{}`), "--safety-scan"); code != ExitBlock {
		t.Errorf("hook Stop --safety-scan = %d, want ExitBlock (guard-like names fail closed)", code)
	}
	if code, _, _ := runFlags("docs-placement", strings.NewReader(`{}`)); code != ExitBlock {
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
	repo := placementRepo(t)
	payload := writePayload(t, "Write", repo, filepath.Join(repo, "NOTES.md"), "# notes\n")
	code, out, errb := runFlags("PreToolUse", strings.NewReader(payload))
	if code != ExitProceed || !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "docs-placement: BLOCKED") {
		t.Errorf("bare hook PreToolUse on a root NOTES.md = %d %q %q, want the docs-placement deny", code, out, errb)
	}
}
