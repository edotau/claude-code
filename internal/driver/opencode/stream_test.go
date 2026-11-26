package opencode

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
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
		{"tools on: the CLI ran them", agent.RunOptions{Tools: true}, fake, false},
		{"prose quoting markup", agent.RunOptions{}, "The fix is to call <invoke name=\"read\"></invoke> before parsing the config file.", false},
		{"plain answer", agent.RunOptions{}, "ok", false},
	} {
		if err := refuseToolCallText("opencode", tc.req, tc.answer); (err != nil) != tc.want {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.want)
		}
	}
}

func TestParseOpenCodeEventsWritten(t *testing.T) {
	ev := func(tool, status, path string) string {
		return `{"type":"tool_use","part":{"tool":"` + tool + `","callID":"` + tool + status + `","state":{"status":"` + status + `","input":{"filePath":"` + path + `"}}}}`
	}
	stream := strings.Join([]string{ev("write", "completed", "/x/b.txt"), ev("edit", "error", "/x/e.txt"), ev("read", "completed", "/x/r.txt")}, "\n")
	res, err := parseOpenCodeEvents(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil, nil, nil)
	if err != nil || !slices.Equal(res.Written, []string{"/x/b.txt"}) {
		t.Fatalf("Written = %v, err = %v; want only /x/b.txt", res.Written, err)
	}
}

// The live 2026-11-22 GPT shape: apply_patch with patchText, and no closing text.
func TestParseOpenCodeEventsApplyPatch(t *testing.T) {
	stream := `{"type":"tool_use","part":{"tool":"apply_patch","callID":"c1","state":{"status":"completed","input":{"patchText":"*** Begin Patch\n*** Add File: notes4-dbg.txt\n+alpha\n*** End Patch"}}}}`
	res, err := parseOpenCodeEvents(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil, nil, nil)
	if err != nil || !slices.Equal(res.Written, []string{"notes4-dbg.txt"}) || res.Answer != agent.NoTextAnswer {
		t.Fatalf("Written = %v, answer = %q, err = %v", res.Written, res.Answer, err)
	}
}

// A step_finish with reason length (the AI SDK's output-limit finish) is an output budget end; a later stop clears it.
func TestParseOpenCodeLengthFinishIsBudget(t *testing.T) {
	fin := func(reason string) string {
		return `{"type":"step_finish","part":{"reason":"` + reason + `","tokens":{"output":5}}}`
	}
	text := `{"type":"text","part":{"text":"partial"}}`
	_, err := parseOpenCodeEvents(bufio.NewReader(strings.NewReader(text+"\n"+fin("length"))), wire.Discard, nil, nil, nil)
	if got := retry.BudgetOf(err); got != retry.BudgetOutput || retry.MayRedispatch(err) {
		t.Fatalf("err = %v, budget %q; want output budget and no redispatch", err, got)
	}
	res, err := parseOpenCodeEvents(bufio.NewReader(strings.NewReader(text+"\n"+fin("length")+"\n"+`{"type":"error","error":{"name":"x"}}`)), wire.Discard, nil, nil, nil)
	if retry.BudgetOf(err) != retry.BudgetOutput || res.Answer != "" {
		t.Fatalf("length then error: err = %v, answer %q; want output budget and no answer", err, res.Answer)
	}
	if _, err := parseOpenCodeEvents(bufio.NewReader(strings.NewReader(fin("length")+"\n"+fin("stop"))), wire.Discard, nil, nil, nil); err != nil {
		t.Fatalf("length then stop: err = %v; want none", err)
	}
}
