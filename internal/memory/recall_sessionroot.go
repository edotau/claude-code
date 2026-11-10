package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// sessionRootLockWait bounds how long a hook waits for a racing repair to finish.
const sessionRootLockWait = 5 * time.Second

func sessionRootPath(sessionID string) string {
	return filepath.Join(StateDir(), "root-"+filepath.Base(sessionID))
}

// SessionRoot uses the launch repository when available, otherwise the hook payload cwd.
func SessionRoot(sessionID, cwd string) string {
	if !LooksLikeSessionID(sessionID) || IsUpdateChild() {
		return cwd
	}
	data, err := os.ReadFile(sessionRootPath(sessionID))
	root := strings.TrimSpace(string(data))
	if err != nil || !filepath.IsAbs(root) {
		return cwd
	}
	return filepath.Clean(root)
}

// sessionRootFor is cwd's git toplevel, else its store origin, else its absolute path.
func sessionRootFor(cwd string) string {
	root := GitToplevel(cwd)
	if root == "" || paths.SamePath(root, StoreDir()) {
		root = Resolve(cwd).Origin
	}
	if root == "" {
		root, _ = filepath.Abs(cwd)
	}
	return root
}

// RecordSessionRoot preserves the first SessionStart root across resume and compaction.
func RecordSessionRoot(sessionID, cwd string) {
	if !LooksLikeSessionID(sessionID) || IsUpdateChild() || SessionRoot(sessionID, "") != "" {
		return
	}
	root := sessionRootFor(cwd)
	if root == "" {
		return
	}
	// Check and write under a per-session lock, rechecking inside it, so racing hooks keep one first root.
	path := sessionRootPath(sessionID)
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		var ran bool
		ran, err = paths.TryFileLock(path+".lock", sessionRootLockWait, func() error {
			if SessionRoot(sessionID, "") != "" {
				return nil
			}
			return paths.AtomicWrite(path, []byte(root+"\n"), 0o600)
		})
		if err == nil && !ran {
			err = fmt.Errorf("%w: %s.lock", paths.ErrLockTimeout, path)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory-start: record session root: %v\n", err)
	}
}
