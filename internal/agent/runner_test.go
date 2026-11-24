package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
)

func TestStampRunnerPiCarriesClaude(t *testing.T) {
	rec := Record{Agent: "claude"}
	res := stampRunner(rec, wire.Result{Agent: "pi", Model: "databricks-grok-4-7", Hops: []wire.Hop{{Agent: "pi"}}})
	if res.Runner != "pi" || res.Asked != "claude" || res.Type != "grok" {
		t.Fatalf("runner %q asked %q type %q", res.Runner, res.Asked, res.Type)
	}
	line := wire.RanOn(res)
	if !strings.HasPrefix(line, "ran on grok via pi · ") || !strings.HasSuffix(line, "· asked claude") {
		t.Fatalf("receipt %q", line)
	}
	rec.Runner, rec.Type = res.Runner, res.Type
	if got := RowLabel(rec); got != "grok via pi" {
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
}
