package recall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

func sessionRootPath(sessionID string) string {
	return filepath.Join(memstore.StateDir(), "root-"+filepath.Base(sessionID))
}

// SessionRoot uses the launch repository when available, otherwise the hook payload cwd.
func SessionRoot(sessionID, cwd string) string {
	if !LooksLikeSessionID(sessionID) || bank.IsUpdateChild() {
		return cwd
	}
	data, err := os.ReadFile(sessionRootPath(sessionID))
	root := strings.TrimSpace(string(data))
	if err != nil || !filepath.IsAbs(root) {
		return cwd
	}
	return filepath.Clean(root)
}

// sessionRootFor is cwd's git toplevel, else its memstore origin, else its absolute path.
func sessionRootFor(cwd string) string {
	root := memstore.GitToplevel(cwd)
	if root == "" || paths.SamePath(root, memstore.StoreDir()) {
		root = memstore.Resolve(cwd).Origin
	}
	if root == "" {
		root, _ = filepath.Abs(cwd)
	}
	return root
}

// RecordSessionRoot preserves the first SessionStart root across resume and compaction.
func RecordSessionRoot(sessionID, cwd string) {
	if !LooksLikeSessionID(sessionID) || bank.IsUpdateChild() || SessionRoot(sessionID, "") != "" {
		return
	}
	root := sessionRootFor(cwd)
	if root == "" {
		return
	}
	// Temp file + hard link: the write is atomic, and a racing hook's first root always wins (no lock file).
	path := sessionRootPath(sessionID)
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	err := paths.AtomicWrite(tmp, []byte(root+"\n"), 0o600)
	if err == nil {
		if err = os.Link(tmp, path); errors.Is(err, fs.ErrExist) {
			err = nil
			if SessionRoot(sessionID, "") == "" { // existing file is corrupt or relative: replace it
				err = os.Rename(tmp, path)
			}
		}
		_ = os.Remove(tmp)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory-start: record session root: %v\n", err)
	}
}
