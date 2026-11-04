package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// "<profile>:<alias>" resolves like --provider <profile> --model <alias>; anything else is left alone.
func TestResolveTargetProfileTierRewrite(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5","haiku":"claude-haiku-4-5"}}}}}`)
	_, want, err := resolveTarget(RunOptions{Provider: "dev", Model: "opus"})
	if err != nil || want.Provider == nil || want.Provider.Name != "dev" {
		t.Fatalf("baseline --provider dev --model opus = %+v, %v", want, err)
	}

	t.Run("dev:opus equals --provider dev --model opus", func(t *testing.T) {
		_, got, err := resolveTarget(RunOptions{Model: "dev:opus"})
		if err != nil || got.Provider == nil || got.Provider.Name != "dev" || got.Model != want.Model {
			t.Errorf("dev:opus = %+v (%v), want provider dev model %q", got, err, want.Model)
		}
	})
	t.Run("full id keeps its prior behaviour", assertFullIDTargetRewrite)
	t.Run("unknown row or non-alias is not rewritten", func(t *testing.T) { assertUnknownTargetRewrite(t, want.Model) })
	t.Run("the explicit backend is still seen", func(t *testing.T) {
		req := RunOptions{Model: "dev:opus"}
		if !ExplicitBackend(req) {
			t.Error("ExplicitBackend(dev:opus) must stay true")
		}
		if acct, _ := explicitAccount(&fakeRunner{name: "claude"}, req); acct != "databricks·dev" {
			t.Errorf("explicitAccount = %q, want databricks·dev", acct)
		}
	})
}

func assertFullIDTargetRewrite(t *testing.T) {
	t.Helper()
	_, viaSpec, err1 := resolveTarget(RunOptions{Model: "dev:claude-opus-5-5"})
	_, viaFlags, err2 := resolveTarget(RunOptions{Provider: "dev", Model: "claude-opus-5-5"})
	if err1 != nil || err2 != nil || viaSpec.Provider == nil || viaSpec.Provider.Name != "dev" ||
		viaSpec.Model != viaFlags.Model {
		t.Errorf("spec %+v (%v) vs flags %+v (%v)", viaSpec, err1, viaFlags, err2)
	}
}

func assertUnknownTargetRewrite(t *testing.T, model string) {
	t.Helper()
	_, bad, err := resolveTarget(RunOptions{Model: "unknownrow:opus"})
	if err == nil && bad.Provider != nil && bad.Provider.Name == "unknownrow" {
		t.Errorf("unknownrow:opus rewritten: %+v", bad)
	}
	_, nt, _ := resolveTarget(RunOptions{Model: "dev:notatier"})
	if nt.Provider != nil && nt.Provider.Name == "dev" && nt.Model == model {
		t.Errorf("dev:notatier rewritten to dev's opus alias: %+v", nt)
	}
}

// handoffRunner returns a canned Result and error per call, so a dispatch can carry a hand-off path.
type handoffRunner struct {
	name string
	res  Result
	err  error
}

func (h *handoffRunner) Name() string     { return h.name }
func (h *handoffRunner) Caps() Caps       { return Caps{} }
func (h *handoffRunner) Available() error { return nil }
func (h *handoffRunner) Run(context.Context, RunOptions, Sink) (Result, error) {
	return h.res, h.err
}

// A hand-off written by dispatch 1 rides the final envelope whichever way the later dispatches end.
func TestRunWithFallbackCarriesHandoff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	first := func() *handoffRunner {
		return &handoffRunner{name: "copilot", res: Result{Handoff: "/tmp/x.md"}, err: quota}
	}

	t.Run("dispatch 2 succeeds", func(t *testing.T) {
		second := &handoffRunner{name: "claude", res: Result{Answer: "from dispatch 2"}}
		res, err := RunWithFallback(context.Background(), chainOf([]Engine{first(), second}, "", "sonnet"), RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Answer != "from dispatch 2" || res.Handoff != "/tmp/x.md" {
			t.Fatalf("res=%+v err=%v; want dispatch 2's answer with handoff /tmp/x.md", res, err)
		}
	})

	t.Run("dispatch 2 fails with another error", func(t *testing.T) {
		boom := errors.New("boom: unrelated failure")
		second := &handoffRunner{name: "claude", err: boom}
		res, err := RunWithFallback(context.Background(), chainOf([]Engine{first(), second}, "", "sonnet"), RunOptions{Prompt: "p"}, Discard, nil)
		if err == nil || !errors.Is(err, boom) || res.Handoff != "/tmp/x.md" {
			t.Fatalf("res=%+v err=%v; want dispatch 2's error with handoff /tmp/x.md", res, err)
		}
	})

	t.Run("a later dispatch's own handoff wins", func(t *testing.T) {
		second := &handoffRunner{name: "claude", res: Result{Answer: "ok", Handoff: "/tmp/y.md"}}
		res, _ := RunWithFallback(context.Background(), chainOf([]Engine{first(), second}, "", "sonnet"), RunOptions{Prompt: "p"}, Discard, nil)
		if res.Handoff != "/tmp/y.md" {
			t.Errorf("handoff = %q, want the later dispatch's /tmp/y.md", res.Handoff)
		}
	})
}

// (4) A dispatch that wrote a hand-off hands the next dispatch the hand-off, not the bare task; an unreadable file changes nothing.
func TestRunWithFallbackNextDispatchGetsHandoff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	file := filepath.Join(t.TempDir(), "h.md")
	if err := os.WriteFile(file, []byte("PARTIAL-PROGRESS-MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"readable": file, "unreadable": file + ".missing"} {
		t.Run(name, func(t *testing.T) {
			second := &fakeRunner{name: "claude", errs: []error{nil}}
			first := &handoffRunner{name: "copilot", res: Result{Handoff: path}, err: quota}
			if _, err := RunWithFallback(context.Background(), chainOf([]Engine{first, second}, "", "sonnet"), RunOptions{Prompt: "BARE-TASK"}, Discard, nil); err != nil {
				t.Fatal(err)
			}
			got := second.seen[0].Prompt
			if name == "readable" && (!strings.Contains(got, "PARTIAL-PROGRESS-MARKER") || strings.Contains(got, "BARE-TASK")) {
				t.Errorf("next dispatch prompt = %q, want the hand-off instead of the task", got)
			}
			if name == "unreadable" && got != "BARE-TASK" {
				t.Errorf("unreadable hand-off must leave the prompt alone, got %q", got)
			}
		})
	}
}
