package copilot

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"

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

func (s *recSink) Emit(ev wire.Event) error { s.types = append(s.types, ev.Type); return nil }

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
		_, _ = copilotPartial(wire.Result{}, out, err, sink)
		return sink.types
	}
	if got := run(rpc.PermissionRequestRead{}); !slices.Equal(got, []string{wire.EventProgress, wire.EventText}) {
		t.Errorf("approved read: events = %v, want [progress text]", got)
	}
	if got := run(rpc.PermissionRequestMCP{}); !slices.Equal(got, []string{wire.EventText}) {
		t.Errorf("denied mcp: events = %v, want [text]", got)
	}
}

// A tool-only run (no text, no error) still marks progress, so a fallback cannot repeat its side effects.
func TestMarkToolRanMarksProgressOnSuccess(t *testing.T) {
	sink := &recSink{}
	if err := markToolRan(SDKResult{ToolRan: true}, sink, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.types) != 1 || sink.types[0] != wire.EventProgress {
		t.Fatalf("sink saw %v, want one progress mark and no tool_use", sink.types)
	}
	if err := markToolRan(SDKResult{}, sink, nil); err != nil || len(sink.types) != 1 {
		t.Fatalf("no tool ran: err=%v events=%v, want none emitted", err, sink.types)
	}
}

// A _TOKEN credential and the SDK token never land in the quota hand-off file, in prompt, partial text or session.
func TestHandoffNeverWritesCredentials(t *testing.T) {
	const sdkToken, envSecret = "gho_SDKTOKEN12345", "dbx_pat_ENVSECRET9"
	dir := t.TempDir()
	orig := handoffDir
	handoffDir = func(string) string { return dir }
	t.Cleanup(func() { handoffDir = orig })
	creds := CredentialValues([]string{"DBX_TOKEN=" + envSecret, "PLAIN=visible"}, sdkToken)
	req := agent.RunOptions{WorkDir: t.TempDir(), Prompt: "use " + sdkToken + " and " + envSecret}
	path, body, err := writeCopilotHandoff(req, wire.Result{SessionID: "sess-1"}, "partial "+envSecret,
		"acct", errors.New("quota exhausted"), creds)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"file": string(onDisk), "returned body": body} {
		if strings.Contains(s, sdkToken) || strings.Contains(s, envSecret) {
			t.Errorf("%s holds a raw credential:\n%s", name, s)
		}
		if !strings.Contains(s, "REDACTED") {
			t.Errorf("%s has no redaction marker:\n%s", name, s)
		}
	}
}

// A failed run keeps the usage the collector summed before the failure.
func TestCopilotPartialKeepsUsage(t *testing.T) {
	want := wire.Usage{InputTokens: 7, OutputTokens: 3}
	res, _ := copilotPartial(wire.Result{}, SDKResult{Text: "partial", Usage: want}, errors.New("boom"), wire.Discard)
	if res.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", res.Usage, want)
	}
}
