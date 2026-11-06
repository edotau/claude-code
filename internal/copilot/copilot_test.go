package copilot

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestResumeSlot(t *testing.T) {
	for in, want := range map[string]string{"sonnet": "sonnet", "dev:haiku": "haiku", "claude-sonnet-5-5": "sonnet", "gpt-5": ""} {
		if got := ResumeAlias(in); got != want {
			t.Errorf("ResumeAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

type recSink struct{ types []string }

func (s *recSink) Emit(ev agent.Event) error { s.types = append(s.types, ev.Type); return nil }

// An approved tool then a 503 leaves the sink with a progress mark before any text; a denied-only run emits none.
func TestToolApprovalMarksProgressOnFailure(t *testing.T) {
	run := func(req copilot.PermissionRequest) []string {
		var ok atomic.Bool
		h := permissionHandler(SDKOptions{AllowAllTools: true, WorkingDirectory: t.TempDir()}, &ok)
		if _, err := h(req, copilot.PermissionInvocation{}); err != nil {
			t.Fatal(err)
		}
		sink := &recSink{}
		out := SDKResult{Text: "part", ToolRan: ok.Load()}
		err := markToolRan(out, sink, errors.New("503 service unavailable")) // Run marks the tool before branching on err
		_, _ = copilotPartial(agent.Result{}, out, err, sink)
		return sink.types
	}
	if got := run(rpc.PermissionRequestRead{}); !slices.Equal(got, []string{agent.EventProgress, agent.EventText}) {
		t.Errorf("approved read: events = %v, want [progress text]", got)
	}
	if got := run(rpc.PermissionRequestMCP{}); !slices.Equal(got, []string{agent.EventText}) {
		t.Errorf("denied mcp: events = %v, want [text]", got)
	}
}

// A tool-only run (no text, no error) still marks progress, so a fallback cannot repeat its side effects.
func TestMarkToolRanMarksProgressOnSuccess(t *testing.T) {
	sink := &recSink{}
	if err := markToolRan(SDKResult{ToolRan: true}, sink, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.types) != 1 || sink.types[0] != agent.EventProgress {
		t.Fatalf("sink saw %v, want one progress mark and no tool_use", sink.types)
	}
	if err := markToolRan(SDKResult{}, sink, nil); err != nil || len(sink.types) != 1 {
		t.Fatalf("no tool ran: err=%v events=%v, want none emitted", err, sink.types)
	}
}

// A failed run keeps the usage the collector summed before the failure.
func TestCopilotPartialKeepsUsage(t *testing.T) {
	want := agent.Usage{InputTokens: 7, OutputTokens: 3}
	res, _ := copilotPartial(agent.Result{}, SDKResult{Text: "partial", Usage: want}, errors.New("boom"), agent.Discard)
	if res.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", res.Usage, want)
	}
}
