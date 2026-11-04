// `hook copilot-bash` — the copilot subagent's frontmatter PreToolUse(Bash) guard: only the scoped shell allowlist runs.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/shellscope"
)

// CopilotBashHook is the `claude-code hook <name>` spelling agents/copilot.md carries; it is not a settings.json event.
const CopilotBashHook = "copilot-bash"

// copilotBashReason names the hook's allowlist, generated from the copilot shell policy.
func copilotBashReason() string {
	p := agentconfig.ShellPolicy()
	return "copilot-bash: BLOCKED — Bash is limited to " + p.Commands.Summary() + ", 2>&1, paths in the cwd or ~/worktrees, " +
		p.HookExtras.Summary() + "."
}

// copilotBash reads one hook JSON from stdin and exits 2 unless every chained segment is allowed (fail closed).
func copilotBash(stdin io.Reader, stderr io.Writer) int {
	var in struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
		CWD string `json:"cwd"`
	}
	raw, _ := io.ReadAll(stdin)
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.ToolInput.Command) == "" {
		fmt.Fprintln(stderr, "copilot-bash: BLOCKED — unreadable hook input or no command.")
		return ExitBlock
	}
	cmd, workDir := in.ToolInput.Command, in.CWD
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	cmd = shellscope.CanonicalSelf(cmd)
	home, _ := os.UserHomeDir() // ~/worktrees holds the linked worktrees a ~/.claude session works in
	if !shellscope.Command(cmd, []string{workDir, filepath.Join(home, "worktrees")}, agentconfig.ShellPolicy().HookExtras) {
		fmt.Fprintln(stderr, copilotBashReason())
		return ExitBlock
	}
	return ExitProceed
}
