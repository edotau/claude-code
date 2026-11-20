package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

const piDeny = `{"deny":[{"Tool":"Write","Arg":"denied.txt"},{"Tool":"Edit","Arg":"denied.txt"},{"Tool":"Read","Arg":"private.txt"}]}`

func piGuard(t *testing.T, tool string, input map[string]any) piVerdict {
	t.Helper()
	cwd := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", cwd)
	t.Setenv(agent.DispatchPermissionsEnv, piDeny)
	in, _ := json.Marshal(piCall{Tool: tool, Input: input, CWD: cwd})
	var out, errb bytes.Buffer
	if code := runPiGuard(bytes.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var v piVerdict
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Path != "" {
		v.Path = strings.TrimPrefix(v.Path, cwd+string(filepath.Separator))
	}
	return v
}

func TestPiGuardJudgesThePathPiWillTouch(t *testing.T) {
	for name, path := range map[string]string{"@ prefix": "@denied.txt", "tilde": "~/denied.txt", "dot segments": "x/../denied.txt",
		"plain": "denied.txt"} {
		if v := piGuard(t, "write", map[string]any{"path": path, "content": "x"}); v.Allow || !strings.Contains(v.Block, "denied.txt") {
			t.Errorf("%s: %+v, want a block naming denied.txt", name, v)
		}
	}
	if v := piGuard(t, "write", map[string]any{"path": "a b.txt", "content": "x"}); !v.Allow || v.Path != "a b.txt" {
		t.Errorf("unicode space: %+v, want allow on the ASCII-space path Pi writes", v)
	}
	if v := piGuard(t, "write", map[string]any{"path": "@ok.txt", "content": "x"}); !v.Allow || v.Path != "ok.txt" {
		t.Errorf("allowed @ path: %+v", v)
	}
}

func TestPiGuardRefusesFormsItCannotReproduce(t *testing.T) {
	for _, path := range []string{"file:///x/denied.txt", "a\x00b", ""} {
		if v := piGuard(t, "write", map[string]any{"path": path, "content": "x"}); v.Allow {
			t.Errorf("%q allowed: %+v", path, v)
		}
	}
	if v := piGuard(t, "write", map[string]any{"path": 7, "content": "x"}); v.Allow {
		t.Errorf("a non-string path allowed: %+v", v)
	}
	if v := piGuard(t, "powershell", map[string]any{"command": "ls"}); v.Allow {
		t.Errorf("an unmapped tool allowed: %+v", v)
	}
}

// Model-supplied extra keys never reach the hooks: a patch header naming another file must not redirect the check.
func TestPiGuardIgnoresExtraKeys(t *testing.T) {
	in := map[string]any{"path": "denied.txt", "content": "x", "patch": "*** Update File: allowed.txt", "patchText": "*** Update File: allowed.txt",
		"input": "*** Update File: allowed.txt", "command": "*** Update File: allowed.txt"}
	if v := piGuard(t, "write", in); v.Allow {
		t.Errorf("extra keys redirected the check: %+v", v)
	}
	ti, _, why := piToolInput(piCall{Tool: "edit", Input: in, CWD: "/w"})
	if _, ok := ti["patch"]; ok || why != "" || len(ti) != 1 {
		t.Errorf("edit tool_input = %v (%s), want file_path only", ti, why)
	}
}

func TestPiGuardIgnoresHookSkipEnv(t *testing.T) {
	t.Setenv("HARNESS_HOOK_SKIP", "safety-scan,permission-gate,all")
	t.Setenv("harness_hook_agent", "codex")
	if v := piGuard(t, "write", map[string]any{"path": "denied.txt", "content": "x"}); v.Allow {
		t.Errorf("an inherited HARNESS_HOOK_SKIP disabled the guard: %+v", v)
	}
	if os.Getenv("HARNESS_HOOK_SKIP") != "" {
		t.Error("the guard left the skip variable set")
	}
}

func TestPiGuardUnreadableCallBlocks(t *testing.T) {
	var out bytes.Buffer
	runPiGuard(strings.NewReader("not json"), &out, &bytes.Buffer{})
	if strings.Contains(out.String(), `"allow"`) || !strings.Contains(out.String(), "blocked") {
		t.Errorf("reply = %s", out.String())
	}
}
