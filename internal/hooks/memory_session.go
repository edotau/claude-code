package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

func sessionRootPath(sessionID string) string {
	return filepath.Join(memstore.StateDir(), "root-"+filepath.Base(sessionID))
}

// sessionRoot uses the launch repository when available, otherwise the hook payload cwd.
func sessionRoot(sessionID, cwd string) string {
	if !memory.LooksLikeSessionID(sessionID) || memory.IsUpdateChild() {
		return cwd
	}
	data, err := os.ReadFile(sessionRootPath(sessionID))
	root := strings.TrimSpace(string(data))
	if err != nil || !filepath.IsAbs(root) {
		return cwd
	}
	return filepath.Clean(root)
}

// recordSessionRoot preserves the first SessionStart root across resume and compaction.
func recordSessionRoot(sessionID, cwd string) {
	if !memory.LooksLikeSessionID(sessionID) || memory.IsUpdateChild() || sessionRoot(sessionID, "") != "" {
		return
	}
	root := memstore.GitToplevel(cwd)
	if root == "" || paths.SamePath(root, memstore.StoreDir()) {
		root = memstore.Resolve(cwd).Origin
	}
	if root == "" {
		root, _ = filepath.Abs(cwd)
	}
	if root == "" {
		return
	}
	if err := paths.AtomicWrite(sessionRootPath(sessionID), []byte(root+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "memory-start: record session root: %v\n", err)
	}
}

func sessionRootInput(in *Input) *Input {
	copy := *in
	copy.CWD = sessionRoot(in.SessionID, in.CWD)
	return &copy
}
