package opencode

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// A tools-off answer that is only tool-call markup (the live 2026-11-05 shape) is an error; prose, or tools on, is not.
func TestRefuseToolCallText(t *testing.T) {
	fake := "<invoke name=\"read\">\n<parameter name=\"filePath\">/x/agentprofile.go</parameter>\n</invoke>\n_tool_use"
	for _, tc := range []struct {
		name   string
		req    agent.RunOptions
		answer string
		want   bool
	}{
		{"tools off, markup only", agent.RunOptions{}, fake, true},
		{"read-only keeps tools on", agent.RunOptions{Tools: true, ReadOnly: true}, fake, false},
		{"tools on: the CLI ran them", agent.RunOptions{Tools: true}, fake, false},
		{"prose quoting markup", agent.RunOptions{}, "The fix is to call <invoke name=\"read\"></invoke> before parsing the config file.", false},
		{"plain answer", agent.RunOptions{}, "ok", false},
	} {
		if err := refuseToolCallText("opencode", tc.req, tc.answer); (err != nil) != tc.want {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.want)
		}
	}
}
