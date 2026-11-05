package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

type attributionScanCase struct {
	name string
	cmd  string
	want core.Decision
}

// attributionScanCases lists footer commits (Ask) and look-alikes (Allow); otherRepo is not the harness tree.
func attributionScanCases(otherRepo string) []attributionScanCase {
	return []attributionScanCase{
		{
			"inline -m footer",
			`git commit -m "feat: x" -m "Co-Authored-By: Claude <noreply@anthropic.com>"`,
			core.Ask,
		},
		{
			"heredoc -m footer",
			"git commit -m \"$(cat <<'EOF'\nfeat: x\n\nCo-Authored-By: Claude Opus <noreply@anthropic.com>\nEOF\n)\"",
			core.Ask,
		},
		{
			"-C other repo, -F heredoc, generated-with footer",
			"git -C " + otherRepo + " commit -F - <<'EOF'\nfeat: x\n\nGenerated with [Claude Code](https://claude.com/claude-code)\nEOF",
			core.Ask,
		},
		{
			"cd into other repo first",
			"cd " + otherRepo + " && git commit -m \"feat: x\" -m \"Co-Authored-By: Claude\"",
			core.Ask,
		},
		{
			"plain commit proceeds",
			`git commit -m "feat: x"`,
			core.Allow,
		},
		{
			"grep for the phrase proceeds (not a commit)",
			`git log --grep "Co-Authored-By: Claude"`,
			core.Allow,
		},
		{
			"echo with no commit proceeds",
			`echo "Co-Authored-By: Claude"`,
			core.Allow,
		},
		{
			"footer naming a human proceeds",
			`git commit -m "feat: x" -m "Co-Authored-By: Ada <a@x>"`,
			core.Allow,
		},
	}
}

// A footer commit in any repo asks with a reason; the rest proceeds.
func TestSafetyScanBlocksAttributionFooterCommit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	otherRepo := t.TempDir() // not the harness tree: the contract is "in any repo"
	for _, c := range attributionScanCases(otherRepo) {
		t.Run(c.name, func(t *testing.T) {
			res := safetyScan(core.ParseInput(bashPayload(c.cmd)))
			if res.Decision != c.want {
				t.Errorf("%q: got %d want %d (reason=%s)", c.cmd, res.Decision, c.want, res.Reason)
			}
			if c.want == core.Ask && !strings.Contains(strings.ToLower(res.Reason), "attribution") {
				t.Errorf("%q: ask reason missing 'attribution': %s", c.cmd, res.Reason)
			}
		})
	}
}

// TestSafetyScanAttributionFooterCopilotShape: the Copilot payload gets the reason as permissionDecisionReason.
func TestSafetyScanAttributionFooterCopilotShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmd := `git commit -m "feat: x" -m "Co-Authored-By: Claude <noreply@anthropic.com>"`
	payload := copilotBashPayload(cmd)
	var out, errb bytes.Buffer
	if got := runDirect("safety-scan", payload, &out, &errb); got != core.ExitBlock {
		t.Fatalf("%q: got %d want ExitBlock (out=%s err=%s)", cmd, got, out.String(), errb.String())
	}
	var doc struct {
		Decision string `json:"permissionDecision"`
		Reason   string `json:"permissionDecisionReason"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Decision != "deny" || !strings.Contains(strings.ToLower(doc.Reason), "attribution") {
		t.Errorf("permissionDecisionReason missing 'attribution': %s", out.String())
	}
}

// copilotBashPayload mirrors the Copilot shape used by TestSafetyScanChecksExecutedHeredocBodiesCopilot.
func copilotBashPayload(cmd string) io.Reader {
	b, _ := json.Marshal(map[string]any{
		"sessionId": "x", "toolName": "Bash", "toolArgs": map[string]string{"command": cmd},
	})
	return strings.NewReader(string(b))
}
