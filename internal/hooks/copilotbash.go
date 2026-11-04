// `hook copilot-bash` — the copilot subagent's frontmatter PreToolUse(Bash) guard: only the scoped shell allowlist runs.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/shellscope"
)

// CopilotBashHook is the `claude-code hook <name>` spelling agents/copilot.md carries; it is not a settings.json event.
const CopilotBashHook = "copilot-bash"

const copilotBashReason = "copilot-bash: BLOCKED — Bash is limited to go test|vet|build|fmt|list|mod tidy, gofmt, goimports, make lint|fmt|test|build, read-only git, git add <paths>, git commit (no --amend, --no-verify), ls cat head tail wc grep rg find, claude-code copilot|worktree merge|test mutate."

// claudeSelf spells the harness binary the way shells and agents write it; each form reads as plain claude-code.
func claudeSelf() []string {
	home, _ := os.UserHomeDir()
	return []string{"$HOME/.claude/bin/claude-code", "~/.claude/bin/claude-code", home + "/.claude/bin/claude-code"}
}

// copilotBashExtra admits the Claude-side verbs the subagent needs beyond the scoped list.
func copilotBashExtra(w []string) bool {
	if len(w) < 2 || w[0] != "claude-code" {
		return false
	}
	return w[1] == "copilot" || (w[1] == "worktree" && len(w) > 2 && w[2] == "merge")
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
	for _, self := range claudeSelf() {
		cmd = strings.ReplaceAll(cmd, self+" ", "claude-code ")
	}
	if !shellscope.Command(cmd, workDir, copilotBashExtra) {
		fmt.Fprintln(stderr, copilotBashReason)
		return ExitBlock
	}
	return ExitProceed
}
