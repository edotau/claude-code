package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
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

func TestPiGuardBadVerifyLineIsUsage(t *testing.T) {
	for _, bad := range []map[string]any{{"verify": "go vet ./x; rm -rf ."}, {"verify": "rm -rf ."}, {"verify": ""}, {"verify": "go vet ./x", "y": 1}} {
		if v := piGuard(t, "verify", bad); v.Allow || v.Usage == "" {
			t.Errorf("%v: verdict = %+v, want a usage error", bad, v)
		}
	}
}

func TestPiVerifyFailureIsRedacted(t *testing.T) {
	if piVerifyTimeout+5*time.Second >= agent.IdleLimit {
		t.Errorf("verify timeout %v does not fit the idle watch %v", piVerifyTimeout, agent.IdleLimit)
	}
	mod := t.TempDir()
	for name, src := range map[string]string{"go.mod": "module p\n", "p.go": "package p\n\nimport \"fmt\"\n\nfunc F() { fmt.Printf(\"%d\\n\", \"x\") }\n"} {
		if err := os.WriteFile(filepath.Join(mod, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	call := piCall{Tool: "verify", Input: map[string]any{"verify": "go vet ./..."}, CWD: mod}
	pr, pw := io.Pipe() // an empty reader would EOF at once and cancel the run
	defer pw.Close()
	v := runPiVerify(call, pr)
	if !v.Allow || !strings.Contains(v.Text, "exit 1") || !strings.Contains(v.Text, "go vet") {
		t.Errorf("verdict = %+v", v)
	}
	redact.StubSource(t, func() ([]string, error) { return nil, errors.New("down") })
	v = runPiVerify(call, pr)
	if !strings.Contains(v.Text, "verify output withheld: redaction unavailable") || strings.Contains(v.Text, "$ go vet") {
		t.Errorf("withheld verdict = %+v", v)
	}
	tail := &piTail{} // a PEM block whose BEGIN the cut drops: none of its body may show
	_, _ = tail.Write([]byte(redact.PEMBegin + "PRIVATE KEY-----\n" + strings.Repeat("QUJD\n", 2*piVerifyKeep/5) + "tail line\n"))
	if got := piVerifyText("go vet ./p", 1, false, 0, tail, false); strings.Contains(got, "QUJD") {
		t.Errorf("PEM body survived the cut: %q", got)
	}
}

func TestPiModulesBlockFailsClosedWhenNoModuleJudges(t *testing.T) {
	if got := piModulesBlock(nil, "Write", []byte("{}")); !strings.Contains(got, "no guard module matched") {
		t.Errorf("empty module list: %q, want a block", got)
	}
}

func saveOwnerContract(t *testing.T, id string, tokens int) {
	t.Helper()
	c, ps := contract.Open(contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		contract.Party{Role: contract.RoleOwner, Agent: "pi"}, contract.Limits{Depth: 1, Tokens: tokens, Leaves: 1},
		contract.Facts{Fleet: true})
	if c, err := c.Start(id); len(ps) > 0 || err != nil || contract.Save(c) != nil {
		t.Fatalf("contract: %v %v", ps, err)
	}
}

func TestPiGuardBudgetNotice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", dir)
	const owner, child, budget = "20261124T100000.000-pi", "20261124T100001.000-pi", 100
	t.Setenv(proc.DispatchIDEnv, owner)
	saveOwnerContract(t, owner, budget)
	at := time.Now().Add(-time.Minute)
	save := func(id, parent string, tokens int) {
		r := agent.Record{ID: id, Time: at, Agent: "pi", Status: "ok", Parent: parent, Usage: &wire.Usage{OutputTokens: tokens}}
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	save(owner, "", 10)
	save(child, owner, 50)
	save("20261124T095959.000-pi", owner, budget*10) // stamped before the root: the scan stops short of it
	bash := piCall{Tool: "bash", Input: map[string]any{"command": "ls"}, CWD: dir}
	if v := judgePi(bash); !v.Allow || v.Notice != "" {
		t.Errorf("tokens under the limit: bash = %+v, want allowed without a notice", v)
	}
	save("20261124T100002.000-pi", child, budget/2)
	if v := judgePi(bash); !v.Allow || v.Notice != contract.BudgetNotice {
		t.Errorf("tokens past the limit: bash = %+v, want the notice", v)
	}
	const holder = "20261124T100009.000-pi"
	if _, err := contract.Update(owner, func(c contract.Contract) (contract.Contract, error) { return c.Reassign(holder) }); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proc.DispatchIDEnv, holder)
	if v := judgePi(bash); v.Notice != contract.BudgetNotice {
		t.Errorf("re-assigned holder: bash = %+v, want the notice", v)
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
