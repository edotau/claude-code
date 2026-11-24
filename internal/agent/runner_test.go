package agent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
)

func TestStampRunnerPiCarriesClaude(t *testing.T) {
	rec := Record{Agent: "claude"}
	res := stampRunner(rec, wire.Result{Agent: "pi", Model: "databricks-grok-4-7", Hops: []wire.Hop{{Agent: "pi"}}})
	if res.Runner != "pi" || res.Asked != "claude" || res.Type != "claude" {
		t.Fatalf("runner %q asked %q type %q", res.Runner, res.Asked, res.Type)
	}
	line := wire.RanOn(res)
	if !strings.HasPrefix(line, "ran on claude via pi · ") {
		t.Fatalf("receipt %q", line)
	}
	rec.Runner, rec.Type = res.Runner, res.Type
	if got := RowLabel(rec); got != "claude via pi" {
		t.Fatalf("row label %q", got)
	}
}

func TestStampRunnerTypedAgentKeepsItsName(t *testing.T) {
	for _, a := range []string{"opencode", "copilot"} {
		own := stampRunner(Record{Agent: a}, wire.Result{Agent: a, Model: "databricks-gpt-6-sol"})
		if own.Type != a || own.Runner != "" {
			t.Fatalf("%s own run: type %q runner %q", a, own.Type, own.Runner)
		}
		pi := stampRunner(Record{Agent: a}, wire.Result{Agent: "pi", Model: "databricks-gpt-6-sol", Hops: []wire.Hop{{Agent: "pi"}}})
		if pi.Type != a || pi.Runner != "pi" {
			t.Fatalf("%s via pi: type %q runner %q", a, pi.Type, pi.Runner)
		}
	}
}

func TestStampRunnerSurvivesWorkspaceRepick(t *testing.T) {
	hops := []wire.Hop{{Agent: "pi"}, {Agent: "pi"}}
	res := stampRunner(Record{Agent: "claude"}, wire.Result{Agent: "pi", Fallback: "claude", Model: "claude-sonnet-5-5", Hops: hops})
	if res.Runner != "pi" || res.Type != "claude" {
		t.Fatalf("runner %q type %q", res.Runner, res.Type)
	}
	hops[0].Agent = "claude"
	if got := stampRunner(Record{Agent: "claude"}, wire.Result{Agent: "pi", Fallback: "claude", Hops: hops}); got.Runner != "" {
		t.Fatalf("a real fallback to pi is no runner: %q", got.Runner)
	}
}

func TestStampRunnerPrefersTypedRunFor(t *testing.T) {
	skipped := wire.Result{Agent: "pi", Fallback: "claude", Hops: []wire.Hop{{Agent: "pi", RunFor: ""}, {Agent: "pi", RunFor: "codex"}}}
	if got := stampRunner(Record{Agent: "claude"}, skipped); got.Runner != "" {
		t.Fatalf("pi answering for codex is no runner of claude: %q", got.Runner)
	}
	skipped.Hops[1].RunFor = "claude"
	if got := stampRunner(Record{Agent: "claude"}, skipped); got.Runner != "pi" {
		t.Fatalf("runner %q", got.Runner)
	}
}

func TestFinishNamesTheRunnerAsRanAs(t *testing.T) {
	rec := Record{Agent: "claude"}
	res := stampRunner(rec, wire.Result{Agent: "pi", Status: wire.StatusOK, Model: "claude-sonnet-5-5", Hops: []wire.Hop{{Agent: "pi"}}})
	rec.Finish(res, nil, 0)
	if rec.RanAs != "pi" || rec.Runner != "pi" || rec.Type != "claude" || rec.Fallback != "" {
		t.Fatalf("ran_as %q runner %q type %q fallback %q", rec.RanAs, rec.Runner, rec.Type, rec.Fallback)
	}
}

func TestLongCompactionIsNotHung(t *testing.T) {
	p := Progress{Stream: true, Phase: phaseCompacting, Quiet: 8 * time.Minute, hungAt: 5 * time.Minute, quietAt: 2 * time.Minute}
	if text, hung := p.signal(); hung || !strings.HasPrefix(text, "compacting") {
		t.Fatalf("signal %q hung %v", text, hung)
	}
}

func TestRowLabelLegacyFallbackNamesTheAnswerer(t *testing.T) {
	if got := RowLabel(Record{Agent: "claude", Fallback: "claude", RanAs: "codex"}); got != "codex" {
		t.Fatalf("row label %q", got)
	}
}

func TestStampRunnerOwnAndCodeMode(t *testing.T) {
	own := stampRunner(Record{Agent: "claude"}, wire.Result{Agent: "claude", Model: "claude-sonnet-5-5"})
	if own.Runner != "" || own.Type != "claude" {
		t.Fatalf("own run: runner %q type %q", own.Runner, own.Type)
	}
	rec := Record{Agent: "pi"}
	rec.Request.Codemode = "on"
	if got := stampRunner(rec, wire.Result{Agent: "pi", Model: "databricks-kimi-k3"}); got.Type != "pi" || got.Runner != "" {
		t.Fatalf("code mode: runner %q type %q", got.Runner, got.Type)
	}
	got := stampRunner(Record{Agent: "pi"}, wire.Result{Agent: "pi", Model: "databricks-kimi-k3"})
	if got.Type != "kimi" || got.Runner != "pi" || got.Asked != "" {
		t.Fatalf("pi ask: runner %q type %q asked %q", got.Runner, got.Type, got.Asked)
	}
}

func TestStampRunnerCodeModeLabelIsPiOnly(t *testing.T) {
	rec := Record{Agent: "claude", Request: RunOptions{Codemode: "on"}}
	if res := stampRunner(rec, wire.Result{Agent: "claude", Model: "claude-sonnet-5-5"}); res.Type != "claude" {
		t.Fatalf("claude code-mode run typed %q", res.Type)
	}
	res := stampRunner(rec, wire.Result{Agent: "pi", Model: "databricks-gpt-6-sol", Hops: []wire.Hop{{Agent: "pi"}}})
	if res.Type != "pi" {
		t.Fatalf("pi code-mode run typed %q", res.Type)
	}
}

func TestCodemodeEnv(t *testing.T) {
	cases := []struct {
		agent, mode string
		want        []string
	}{
		{"claude", "on", []string{"CODEMODE=on"}},
		{"opencode", "on", []string{"CODEMODE=on", "OPENCODE_EXPERIMENTAL_CODE_MODE=on"}},
		{"opencode", "off", nil},
		{"opencode", "", nil},
		{"codex", "on", nil},
		{"pi", "on", nil},
	}
	for _, c := range cases {
		if got := CodemodeEnv(c.agent, c.mode); !slices.Equal(got, c.want) {
			t.Errorf("CodemodeEnv(%s,%s) = %v, want %v", c.agent, c.mode, got, c.want)
		}
	}
}
