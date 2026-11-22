package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestDispatchRunReceiptGrammar(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeDispatch(t, leafJob, record(leafJob, "ok", 0))
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

// TestOpeningAskWords: the ask's words end with its simple command; a substitution stays one word.
func TestOpeningAskWords(t *testing.T) {
	ask := []string{"claude-code", "agents", "ask", "x"}
	std := strings.Fields("claude-code agents ask --agent codex --provider databricks --model gpt-5 --tools --timeout 30m --workdir /w - < /b.md")
	for _, c := range []struct {
		brief string
		want  []string
	}{
		{"Run: claude-code agents ask x && echo done", ask},
		{"Run: claude-code agents ask x; echo done", ask},
		{"Run: claude-code agents ask x | cat", ask},
		{"Run: (claude-code agents ask x)", ask},
		{"Run: claude-code agents ask $(echo x)", []string{"claude-code", "agents", "ask", "$(echo x)"}},
		{"Run: claude-code agents ask $(printf x)", []string{"claude-code", "agents", "ask", "$(printf x)"}},
		{"Run: claude-code agents ask <(printf x)", []string{"claude-code", "agents", "ask", "<(printf x)"}},
		{"Run: claude-code \\\nagents ask x", nil}, // the Run: line is the brief's first line
		{"Run: echo $(claude-code agents ask x)", nil},
		{"Run: ( claude-code agents ask x )", nil},
		{"Run: " + strings.Join(std, " "), std},
		{"Run: claude-code agents ask --agent codex - < /b.md", strings.Fields("claude-code agents ask --agent codex - < /b.md")},
		{"Run: rtk claude-code gemini ask --tools 'do it now'", []string{"claude-code", "gemini", "ask", "--tools", "do it now"}},
		{"Run: claude-code codex ask fix it", strings.Fields("claude-code codex ask fix it")},
	} {
		if got := openingAskWords(c.brief); !slices.Equal(got, c.want) {
			t.Errorf("openingAskWords(%q) = %q, want %q", c.brief, got, c.want)
		}
	}
}

// checkReceiptCommand expects the ask command to parse as an opening launch and to earn its run receipt and label.
func checkReceiptCommand(t *testing.T, command, verb string, local bool) {
	t.Helper()
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
	e := &effortPlan{updated: map[string]any{"subagent_type": "claude", "description": "inspect hook"}, run: newDispatchRun(nil)}
	if e.label(context.Background(), "prompt", brief); e.updated["description"] != "pinned: inspect hook" || !e.changed {
		t.Fatalf("missing label rewrite for %q: %+v", brief, e.updated)
	}
}

// The hook matches AcceptModel on the raw model: a `<profile>:` prefix is refused as Prefixed, whatever the fallback.
func TestAskLineRefusalMatchesAcceptModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	if err := serving.Save(serving.Served{Profile: "dev", FetchedAt: time.Now(), Models: []serving.ServedModel{
		{ID: "databricks-claude-opus-5-5", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	reg, err := newDispatchRun(nil).registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"claude-opus-5-9", "dev:claude-opus-5-9", "dev:opus"} {
		for _, fallback := range []string{"none", "agents"} {
			flags := "--model " + model + " --fallback " + fallback
			if !strings.Contains(model, ":") {
				flags += " --profile dev"
			}
			l := parseAskLine(t.Context(), "Run: claude-code agents ask --agent claude "+flags+" 'x'")
			checkAskRefusal(t, l.refusal(reg, "claude"), reg, model, fallback)
		}
	}
}

func checkAskRefusal(t *testing.T, got error, reg *providers.Registry, model, fallback string) {
	t.Helper()
	_, want := agentpkg.AcceptModel("claude", reg.Providers["dev"], model, time.Now())
	var ue *agentpkg.UnacceptedError
	switch {
	case strings.Contains(model, ":"):
		if want == nil || !errors.As(got, &ue) || ue.Kind != agentpkg.UnacceptedPrefixed || !strings.Contains(got.Error(), `--overlay '{"provider"`) {
			t.Errorf("--model %s --fallback %s: refusal = %v, want Prefixed with the overlay form", model, fallback, got)
		}
	case fallback == "none" && fmt.Sprint(got) != fmt.Sprint(want):
		t.Errorf("--model %s: refusal = %v, want %v", model, got, want)
	}
}
