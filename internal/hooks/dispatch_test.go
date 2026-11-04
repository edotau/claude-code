package hooks

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
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
		if _, ok := Native[s.Name]; !ok {
			t.Errorf("no handler for %s", s.Name)
			continue
		}
		var out, errw bytes.Buffer
		if code := RunArgs(s.Event, []string{"--" + s.Name}, strings.NewReader(`{}`), &out, &errw); code == 1 && strings.Contains(errw.String(), "no module") {
			t.Errorf("%s: no module on %s", s.Name, s.Event)
		}
	}
}

// A gate group stays on even when the vault holds a switch-off that SaveHarnessHooks would have refused.
func TestHarnessGateGroupIgnoresSwitchOff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	err := vault.Update(func(tx *vault.Tx) error {
		return tx.Put(vault.Overlay, "harnessHooks", `{"safety":false,"plans":false}`)
	})
	if err != nil {
		t.Skipf("hermetic vault unavailable: %v", err)
	}
	on := enabledGroups()
	if !on("safety", true) {
		t.Error("gate group must ignore harnessHooks=false")
	}
	if on("docs", false) {
		t.Error("non-gate group must honor harnessHooks=false, a stored plans switch read as docs")
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

func TestDispatchBlockingGateFailsClosedOnPanic(t *testing.T) {
	gate := bound("gate", "Bash", time.Second, func(*core.Input) core.Result { panic("scan blew up") })
	gate.Spec.Blocks = true
	res, _ := Dispatch([]Bound{gate}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if res.Decision != core.Block || !strings.Contains(res.Reason, "gate") {
		t.Errorf("panicking Blocks:true handler must block, got %+v", res)
	}
}
