package client

import (
	"io"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent"
)

func newAskTarget() *AskTarget {
	s := func() *string { return new(string) }
	b := func() *bool { return new(bool) }
	return &AskTarget{Name: s(), Provider: s(), Model: s(), Effort: s(), Workdir: s(), Session: s(),
		Tools: b(), ReadOnly: b(), NoTools: b(), Autonomous: b(), Fleet: b(), EnableMemory: b(), Direct: b(),
		NoPersist: b(), SafeMode: b(), DebugFile: s(), SystemPrompt: s(), AppendSystemPrompt: s(), OutputSchema: s(),
		Host: s(), RemoteBin: s(), Fallback: s(), Timeout: new(time.Duration), Attach: new([]string), KeepEnv: new([]string),
		PermMode: s(), PermAllow: new([]string), PermAsk: new([]string), PermDeny: new([]string)}
}

// A retried or backgrounded dispatch must keep --no-tools; without it the tools come back on.
func TestApplyRecordRoundTripsNoTools(t *testing.T) {
	s := newAskTarget()
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{NoTools: true}}, io.Discard)
	if !*s.NoTools || *s.Tools {
		t.Errorf("noTools=%v tools=%v; want noTools on, tools off", *s.NoTools, *s.Tools)
	}
	// Negative control: a record without NoTools clears an earlier value.
	*s.NoTools = true
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}}, io.Discard)
	if *s.NoTools || !*s.Tools {
		t.Errorf("noTools=%v tools=%v; want tools on, noTools off", *s.NoTools, *s.Tools)
	}
}

// A replayed read-only dispatch must stay read-only; the flag default would hand it write tools.
func TestApplyRecordRoundTripsReadOnly(t *testing.T) {
	s := newAskTarget()
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{ReadOnly: true, Tools: true}}, io.Discard)
	if !*s.ReadOnly || !*s.Tools {
		t.Errorf("readOnly=%v tools=%v; want both on", *s.ReadOnly, *s.Tools)
	}
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}}, io.Discard)
	if *s.ReadOnly {
		t.Error("readOnly stayed on for a record without it")
	}
}
