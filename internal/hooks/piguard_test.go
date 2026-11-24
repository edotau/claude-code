package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func piGuard(t *testing.T, tool string, input map[string]any) piVerdict {
	t.Helper()
	return piGuardIn(t, t.TempDir(), tool, input)
}

func piGuardIn(t *testing.T, cwd, tool string, input map[string]any) piVerdict {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", cwd)
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

func TestPiGuardIgnoresHookSkipEnv(t *testing.T) {
	t.Setenv("HARNESS_HOOK_SKIP", "safety-scan,all")
	t.Setenv("harness_hook_agent", "codex")
	cmd := `git commit -m x -m "Co-Authored-By: Claude <noreply@anthropic.com>"`
	if v := piGuard(t, "bash", map[string]any{"command": cmd}); v.Allow {
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

func TestPiModulesBlockFailsClosedWhenNoModuleJudges(t *testing.T) {
	if got := piModulesBlock(nil, "Write", []byte("{}")); !strings.Contains(got, "no guard module matched") {
		t.Errorf("empty module list: %q, want a block", got)
	}
}
