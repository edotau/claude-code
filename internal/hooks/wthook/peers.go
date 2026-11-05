package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// peersOf runs the peerAgents scan for top under ctx's deadline; a scan past it is abandoned as an error.
func peersOf(ctx context.Context, top string, nested []string) ([]worktree.Proc, error) {
	type scan struct {
		procs []worktree.Proc
		err   error
	}
	done := make(chan scan, 1)
	go func() {
		procs, err := peerAgents(top, nested)
		done <- scan{procs, err}
	}()
	select {
	case s := <-done:
		return s.procs, s.err
	case <-ctx.Done():
		return nil, fmt.Errorf("process scan: %w", ctx.Err())
	}
}

// sessionLabel names a peer from Claude Code's session registry (<config>/sessions/<pid>.json; never the .key file).
func sessionLabel(p worktree.Proc) string {
	fallback := fmt.Sprintf("%s pid %d", p.Comm, p.PID)
	b, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "sessions", strconv.Itoa(p.PID)+".json"))
	if err != nil {
		return fallback
	}
	var s struct {
		Name      string `json:"name"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(b, &s) != nil || len(s.SessionID) < 8 {
		return fallback
	}
	name := s.Name
	if name == "" {
		name = s.SessionID[:8]
	}
	return fmt.Sprintf("session %s (%s, pid %d)", name, s.SessionID[:8], p.PID)
}
