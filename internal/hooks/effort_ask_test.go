package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
)

func TestDispatchRunReceiptGrammar(t *testing.T) {
	stubResolve(t, func(q agentpkg.ModelQuery) string { return q.Model }) // grammar under test, not the route of "pinned"
	for _, prefix := range []string{"", "FLAG=1 ", "timeout 30 ", "nohup ", "env FLAG=1 ", "time command ", "env FLAG=1 timeout 30 "} {
		for _, cli := range []string{"claude-code", "/opt/bin/claude-code"} {
			for _, verb := range []string{"agents ask --agent codex", "codex ask", "copilot ask", "gemini ask", "opencode ask"} {
				for _, remote := range []string{"", "--remote host "} {
					checkReceiptCommand(t, prefix+cli+" "+remote+verb+" --model pinned --fallback none 'inspect the hook'", verb, remote == "")
				}
			}
		}
	}
	for _, flags := range []string{"--model x", "--profile p", "--overlay -", "-", "--tools"} {
		if got := parseAskLine(t.Context(), "Run: claude-code agents ask "+flags).prompt; got != "" {
			t.Errorf("%s: prompt = %q, want empty", flags, got)
		}
	}

	for _, brief := range []string{"Run: echo 'claude-code codex ask x'", "Run: claude-code agents result id", "Run: claude-code agents result -f id", "Task\nRun: claude-code codex ask x"} {
		if opensWithAsk(brief) {
			t.Errorf("not an opening launch: %q", brief)
		}
	}
}

// checkReceiptCommand expects the ask command to parse as an opening launch and to earn its run receipt and label.
func checkReceiptCommand(t *testing.T, command, verb string, local bool) {
	t.Helper()
	if !askCmdRe().MatchString(command) {
		t.Fatalf("receipt rejects %q", command)
	}
	brief := "Run: " + command
	l := parseAskLine(context.Background(), brief)
	if !opensWithAsk(brief) || l.model != "pinned" || l.prompt != "inspect the hook" || l.local != local {
		t.Fatalf("%q: parsed %+v", brief, l)
	}
	wantAgent := strings.Fields(verb)[0]
	if wantAgent == "agents" {
		wantAgent = "codex"
	}
	if l.agent != wantAgent {
		t.Fatalf("%q: agent %q", command, l.agent)
	}
	checkReceiptEarned(t, command, brief)
}

// checkReceiptEarned expects a run of command to leave a receipt and the label rule to hold for brief.
func checkReceiptEarned(t *testing.T, command, brief string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, leafJob, record(leafJob, "ok", 0))
	var scan leafScan
	content, err := json.Marshal([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]string{"command": command}}})
	if err != nil {
		t.Fatal(err)
	}
	scan.addCalls(content, time.Time{})
	scan.scanResult(leafToolResult{ToolUseID: "call", Content: json.RawMessage(`"ask: ran on codex · pinned\nask: dispatch ` + leafJob + `"`)})
	if !scan.ranOn {
		t.Fatalf("no receipt for %q", command)
	}
	e := &effortPlan{updated: map[string]any{"subagent_type": "codex", "description": "inspect hook"}, run: newDispatchRun(nil)}
	if e.label(context.Background(), brief); e.updated["description"] != "pinned: inspect hook" || !e.changed {
		t.Fatalf("missing label rewrite for %q: %+v", brief, e.updated)
	}
}
