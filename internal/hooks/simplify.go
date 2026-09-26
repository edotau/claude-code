package hooks

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/transcript"
)

// skipSimplifyAgents are read-only or config agents: an edit from one is never code to simplify.
var skipSimplifyAgents = map[string]bool{
	"Explore": true, "Plan": true, "gemini": true, "claude-code-guide": true, "statusline-setup": true,
}

// SimplifyAfterEdits blocks a subagent's FIRST stop when it edited source code, sending it back to run the
// simplicity pass on those files. Exit 2 on SubagentStop keeps the subagent going with stderr as its next input.
func SimplifyAfterEdits(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if in.StopHookActive || in.AgentTranscriptPath == "" {
		return ExitProceed
	}
	agentType := cmp.Or(in.nested("agent_type"), agentTypeFromMeta(in.AgentTranscriptPath))
	if skipSimplifyAgents[agentType] {
		return ExitProceed
	}
	// One pass per subagent: the flag outlives the block, so the second stop proceeds.
	flag := filepath.Join(paths.StateDir(), "simplified", safeMarkerID(cmp.Or(in.SubagentID(), agentType)))
	if _, err := os.Stat(flag); err == nil {
		return ExitProceed
	}
	files := codeFiles(transcript.EditedFiles(in.AgentTranscriptPath, 50), 20)
	if len(files) == 0 {
		return ExitProceed
	}
	_ = os.MkdirAll(filepath.Dir(flag), 0o700)
	_ = os.WriteFile(flag, nil, 0o600)
	fmt.Fprintf(stderr, "You modified source code. Before finishing, run the simplicity pass on it: read "+
		"%s and apply its checklist (reuse, needless abstraction, duplicate logic, dead code), and run "+
		"`claude-code review quality <file>` on each (Go, Python, JS/TS). Make behavior-preserving fixes only, then finish. Files:\n%s\n",
		filepath.Join(paths.ConfigDir(), "skills", "simplicity", "SKILL.md"), strings.Join(files, "\n"))
	return ExitBlock
}

// codeFiles keeps up to limit paths whose extension the review detectors treat as source.
func codeFiles(files []string, limit int) []string {
	var out []string
	for _, f := range files {
		if isSourceExt(filepath.Ext(f)) && len(out) < limit {
			out = append(out, f)
		}
	}
	return out
}

func isSourceExt(ext string) bool {
	for _, exts := range source.Extensions {
		for _, e := range exts {
			if e == ext {
				return true
			}
		}
	}
	return false
}

// agentTypeFromMeta reads agentType from the <transcript>.meta.json Claude Code writes beside a subagent transcript.
func agentTypeFromMeta(transcriptPath string) string {
	raw, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var meta struct {
		AgentType string `json:"agentType"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.AgentType
}
