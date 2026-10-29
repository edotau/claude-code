package cli

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

func newAskState() *askState {
	s := func() *string { return new(string) }
	b := func() *bool { return new(bool) }
	return &askState{name: s(), provider: s(), model: s(), effort: s(), workdir: s(), session: s(), rubric: s(),
		tools: b(), noTools: b(), autonomous: b(), enableMemory: b(), direct: b(),
		noPersist: b(), safeMode: b(), debugFile: s(), systemPrompt: s(), appendSystemPrompt: s(), outputSchema: s(),
		timeout: new(time.Duration), attach: new(attachFlag), keepEnv: new(keepEnvFlag)}
}

// A retried or backgrounded dispatch must keep --no-tools; without it the tools come back on.
func TestApplyRecordRoundTripsNoTools(t *testing.T) {
	s := newAskState()
	s.applyRecord(agent.Record{Agent: "claude", Request: agent.Request{NoTools: true}})
	if !*s.noTools || *s.tools {
		t.Errorf("noTools=%v tools=%v; want noTools on, tools off", *s.noTools, *s.tools)
	}
	// Negative control: a record without NoTools clears an earlier value.
	*s.noTools = true
	s.applyRecord(agent.Record{Agent: "claude", Request: agent.Request{Tools: true}})
	if *s.noTools || !*s.tools {
		t.Errorf("noTools=%v tools=%v; want tools on, noTools off", *s.noTools, *s.tools)
	}
}
