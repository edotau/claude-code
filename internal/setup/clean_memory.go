package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// orphanAccessLog allows only the two legacy names without a corresponding store.
func orphanAccessLog(p string) bool {
	if filepath.Dir(p) != filepath.Clean(memory.AccessStateDir()) {
		return false
	}
	name := filepath.Base(p)
	if name != "global.log" && name != "memories.log" {
		return false
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	_, err := os.Lstat(filepath.Join(memory.StoreDir(), strings.TrimSuffix(name, ".log")))
	return os.IsNotExist(err)
}

// openMemoryFiles is a var so tests can stub the process-table scan (production: scanOpenMemoryFiles).
var openMemoryFiles = scanOpenMemoryFiles

// memoryStateKeeper snapshots live owners once per sweep and keeps uncertain state.
func memoryStateKeeper() func(string) bool {
	var sessions, opened map[string]bool
	var checked, known bool
	return func(p string) bool {
		if !checked {
			sessions, known = liveMemorySessions()
			var ok bool
			opened, ok = openMemoryFiles()
			known = known && ok
			checked = true
		}
		if !known || opened[p] || opened[p+".lock"] {
			return true
		}
		return sessions[memoryStateSession(p)]
	}
}

// liveMemorySessions treats an unreadable sessions dir or live registry entry as unknown ownership.
func liveMemorySessions() (map[string]bool, bool) {
	dir := filepath.Join(paths.ConfigDir(), "sessions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, true
	}
	if err != nil {
		return nil, false
	}
	live := map[string]bool{}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !strings.HasSuffix(e.Name(), ".json") || !keepLiveSession(p) {
			continue
		}
		var entry struct {
			SessionID string `json:"sessionId"`
		}
		data, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(data, &entry) != nil || entry.SessionID == "" {
			return nil, false
		}
		live[entry.SessionID] = true
	}
	return live, true
}

// memoryStateSession extracts only state families whose names encode a session ID.
func memoryStateSession(p string) string {
	name := filepath.Base(p)
	if filepath.Base(filepath.Dir(p)) == "recall-seen" {
		return name
	}
	for _, prefix := range []string{"stamp-", "count-", "root-", "update-result-", "watch-"} {
		if sid, ok := strings.CutPrefix(name, prefix); ok {
			return strings.TrimSuffix(sid, ".log")
		}
	}
	return ""
}

// sandboxPrefix and sandboxSeed mirror what internal/memory/sandbox.go writes: the seed commit of <dir>/memory
// (author + subject) is the provenance; the owner marker only says whether a process still uses the dir.
const (
	sandboxPrefix  = "claude-memory-sandbox-"
	sandboxSeed    = "Memory sandbox\x1fchore(memory): seed sandbox"
	sandboxForeign = "not a sandbox"
)

// sandboxGlobs matches the sandbox dirs under the OS temp dir plus every live registry entry elsewhere.
func sandboxGlobs() []string {
	temp := filepath.Clean(os.TempDir())
	globs := []string{filepath.Join(temp, sandboxPrefix+"*")}
	dirs, _ := memory.UpdateSandboxRegistry("")
	for _, d := range dirs {
		if filepath.Dir(d) != temp && !strings.ContainsAny(d, "*?[") && (filepath.Separator == '\\' || !strings.Contains(d, `\`)) {
			globs = append(globs, d)
		}
	}
	return globs
}

// keepNonSandbox protects every match that is not a disposable memory sandbox; clean removes only the rest.
func keepNonSandbox(p string) bool { return sandboxKeep(p) != "" }

// sandboxNote names why a genuine sandbox is kept; a non-sandbox gets no note.
func sandboxNote(p string) string {
	if why := sandboxKeep(p); why != sandboxForeign {
		return why
	}
	return ""
}

// sandboxKeep is "" for a disposable sandbox, else the reason it stays: any doubt keeps it.
func sandboxKeep(p string) string {
	if !isSandbox(p) {
		return sandboxForeign
	}
	if sandboxInUse(p) {
		return "in use"
	}
	if sandboxModified(filepath.Join(p, "memory")) {
		return "modified"
	}
	return ""
}

// isSandbox is true for a real (non-symlink) sandbox-named dir outside the live config dir whose memory/ repo
// starts with the sandbox seed commit.
func isSandbox(p string) bool {
	p = filepath.Clean(p)
	if !strings.HasPrefix(filepath.Base(p), sandboxPrefix) || paths.Within(p, paths.ConfigDir()) {
		return false
	}
	if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
		return false
	}
	mem := filepath.Join(p, "memory")
	if fi, err := os.Lstat(filepath.Join(mem, ".git")); err != nil || !fi.IsDir() {
		return false
	}
	root, err := sandboxGit(mem, "log", "--max-parents=0", "--format=%an%x1f%s")
	return err == nil && root == sandboxSeed
}

// sandboxGit runs git in a sandbox's memory repo under a timeout.
func sandboxGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return proc.RunGit(ctx, dir, args...)
}

// sandboxInUse is true when the owner marker names a live process or cannot be read; no marker means age only.
func sandboxInUse(p string) bool {
	data, err := os.ReadFile(filepath.Join(p, memory.SandboxOwnerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	var owner memory.SandboxOwner
	return err != nil || json.Unmarshal(data, &owner) != nil || pidAlive(owner.PID)
}

// sandboxModified is false only when HEAD is the one seed commit and the tree has no change, untracked or ignored file.
func sandboxModified(mem string) bool {
	count, err := sandboxGit(mem, "rev-list", "--count", "HEAD")
	if err != nil || count != "1" {
		return true
	}
	status, err := sandboxGit(mem, "--no-optional-locks", "status", "--porcelain", "--ignored") // no index refresh: it would age the dir
	return err != nil || status != ""
}
