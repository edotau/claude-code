package setup

import (
	"encoding/json"
	"errors"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
