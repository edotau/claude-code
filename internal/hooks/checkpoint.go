package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// checkpointPct is the context-usage share that blocks Stop once per session.
const checkpointPct = 85

// ContextCheckpoint blocks Stop once per session at ≥85% context so the model can hand off before compaction.
func ContextCheckpoint(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if in.SessionID == "" || in.TranscriptPath == "" || in.StopHookActive {
		return ExitProceed
	}
	flag := filepath.Join(paths.StateDir(), "checkpoint", filepath.Base(in.SessionID))
	if _, err := os.Stat(flag); err == nil {
		return ExitProceed
	}
	u, ok := transcript.LastUsage(in.TranscriptPath)
	if !ok {
		return ExitProceed
	}
	pct := u.Tokens * 100 / transcript.Window(u.Model, u.Tokens)
	if pct < checkpointPct {
		return ExitProceed
	}
	_ = os.MkdirAll(filepath.Dir(flag), 0o700)
	_ = os.WriteFile(flag, nil, 0o600)
	fmt.Fprintf(stderr, "Context usage ~%d%% (≥%d%%). Before stopping, write a short handoff of open work and decisions (or run /compact) so nothing is lost to compaction.\n", pct, checkpointPct)
	return ExitBlock
}
