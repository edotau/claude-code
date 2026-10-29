package hooks

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/vault"
)

func bound(group, matcher string, budget time.Duration, run Runner) Bound {
	return Bound{Group: group, Spec: hookspec.EventSpec{Name: group, Event: "PreToolUse", Matcher: matcher, Budget: budget}, Run: run}
}

func bashInput() *Input { return ParseInput(strings.NewReader(`{"tool_name":"Bash","hook_event_name":"PreToolUse"}`)) }

func TestDispatchMergeAndIsolation(t *testing.T) {
	var order []string
	rec := func(n string, r Result) Runner { return func(*Input) Result { order = append(order, n); return r } }
	bs := []Bound{
		bound("a", "Bash", time.Second, rec("a", Result{Decision: Ask, Reason: "why", Context: "ctxA", SystemMessage: "sa"})),
		bound("boom", "Bash", time.Second, func(*Input) Result { panic("x") }),
		bound("slow", "Bash", 20*time.Millisecond, func(*Input) Result { time.Sleep(300 * time.Millisecond); return Result{Decision: Block} }),
		bound("c", "Bash", time.Second, rec("c", Result{Decision: Allow, Context: "ctxC", SystemMessage: "sc"})),
		bound("skipped", "Write", time.Second, rec("skipped", Result{Decision: Block})),
	}
	var log bytes.Buffer
	res, err := Dispatch(bs, "PreToolUse", bashInput(), &log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "a,c" {
		t.Errorf("order or matcher filtering: %v", order)
	}
	if res.Decision != Ask || res.Reason != "why" {
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
		bound("ask", "", time.Second, func(*Input) Result { return Result{Decision: Ask} }),
		bound("block", "", time.Second, func(*Input) Result { return Result{Decision: Block, Reason: "no"} }),
		bound("allow", "", time.Second, func(*Input) Result { return Result{} }),
	}
	res, _ := Dispatch(bs, "PreToolUse", bashInput(), &bytes.Buffer{})
	if res.Decision != Block || res.Reason != "no" {
		t.Errorf("got %+v", res)
	}
}

func TestDispatchSecondUpdatedInputAndRawStdout(t *testing.T) {
	up := func(v int) Runner { return func(*Input) Result { return Result{UpdatedInput: map[string]any{"k": v}} } }
	res, err := Dispatch([]Bound{bound("u1", "", time.Second, up(1)), bound("u2", "", time.Second, up(2))}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if err == nil || res.UpdatedInput["k"] != 1 {
		t.Errorf("second updatedInput must error and be dropped: %v %+v", err, res)
	}
	raw := func(*Input) Result { return Result{Stdout: "RAW"} }
	res, err = Dispatch([]Bound{bound("other", "", time.Second, raw)}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if err == nil || res.Stdout != "" {
		t.Errorf("non-worktree raw stdout must error and drop: %v %q", err, res.Stdout)
	}
	res, err = Dispatch([]Bound{bound("worktree", "", time.Second, raw)}, "PreToolUse", bashInput(), &bytes.Buffer{})
	if err != nil || res.Stdout != "RAW" {
		t.Errorf("worktree may write raw stdout: %v %q", err, res.Stdout)
	}
}

// Every declared hook name still resolves through the legacy verb table.
func TestContractLegacyNamesResolve(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, s := range hookspec.Specs() {
		if _, ok := Registry[s.Name]; !ok {
			t.Errorf("no handler for %s", s.Name)
			continue
		}
		var out, errw bytes.Buffer
		if code := Run(s.Name, strings.NewReader(`{}`), &out, &errw); code == 1 && strings.Contains(errw.String(), "unknown hook") {
			t.Errorf("%s: unknown hook", s.Name)
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
	if on("plans", false) {
		t.Error("non-gate group must honor harnessHooks=false")
	}
}
