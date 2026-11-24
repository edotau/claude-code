package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/proc"
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

func TestPiGuardBudgetNotice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", dir)
	const owner, child, budget = "20261124T100000.000-pi", "20261124T100001.000-pi", 100
	t.Setenv(proc.DispatchIDEnv, owner)
	c, ps := contract.Open(contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		contract.Party{Role: contract.RoleOwner, Agent: "pi"}, contract.Limits{Depth: 1, Tokens: budget, Leaves: 1},
		contract.Facts{Fleet: true})
	if c, err := c.Start(owner); len(ps) > 0 || err != nil || contract.Save(c) != nil {
		t.Fatalf("contract: %v %v", ps, err)
	}
	at := time.Now().Add(-time.Minute)
	save := func(id, parent string, tokens int) {
		r := agent.Record{ID: id, Time: at, Agent: "pi", Status: "ok", Parent: parent, Usage: &wire.Usage{OutputTokens: tokens}}
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	save(owner, "", 10)
	save(child, owner, 50)
	bash := piCall{Tool: "bash", Input: map[string]any{"command": "ls"}, CWD: dir}
	if v := judgePi(bash); !v.Allow || v.Notice != "" {
		t.Errorf("leaves spent, tokens under: bash = %+v, want allowed without a notice", v)
	}
	save("20261124T100002.000-pi", child, budget/2)
	if v := judgePi(bash); !v.Allow || v.Notice != contract.BudgetNotice {
		t.Errorf("tokens past the limit: bash = %+v, want the notice", v)
	}
}

func TestPiActionArgvIsBuiltByGo(t *testing.T) {
	got, err := piActionArgv("gate", map[string]any{"scope": []any{"internal/pi", "cmd"}})
	if want := "worktree gate --run --format json --scope internal/pi,cmd"; err != nil || strings.Join(got, " ") != want {
		t.Errorf("argv = %v, %v", got, err)
	}
	for _, in := range []map[string]any{{"scope": []any{"../x"}}, {"scope": []any{"/etc"}}, {"scope": []any{"a b"}}, {"x": 1}} {
		if _, err := piActionArgv("gate", in); err == nil {
			t.Errorf("gate %v: want a usage error", in)
		}
	}
	if v := piGuard(t, "handback", map[string]any{"status": "DONE"}); !v.Allow || v.Run != nil {
		t.Errorf("handback = %+v", v)
	}
	if v := piGuard(t, "handback", map[string]any{"nope": 1}); v.Allow || v.Block != "" || v.Usage == "" {
		t.Errorf("unknown handback key = %+v", v)
	}
}
