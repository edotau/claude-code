package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
)

// pruneDispatches trims the oldest records past dispatchKeep, never the ids in keep nor an active record.
func pruneDispatches(dir string, keep ...string) {
	names, err := recordNames(dir)
	if err != nil || len(names) <= dispatchKeep+dispatchPruneMargin { // best-effort: a failed listing skips this prune
		return
	}
	excess := len(names) - dispatchKeep
	for _, name := range names {
		if excess <= 0 {
			return
		}
		if slices.Contains(keep, strings.TrimSuffix(name, ".json")) || activeRecord(filepath.Join(dir, name)) {
			continue
		}
		if !removeRecord(dir, name) {
			continue
		}
		excess--
	}
}

// removeRecord deletes one dispatch record and its files under its lock; false means it was held or revived.
func removeRecord(dir, name string) bool {
	removed := false
	// best-effort: a lock that cannot be opened skips this record, as a held one does
	ran, _ := paths.TryFileLock(filepath.Join(dir, name+".lock"), 0, func() error {
		if activeRecord(filepath.Join(dir, name)) { // a claim or update may land between the caller's check and the lock
			return nil
		}
		removeRecordFiles(dir, name)
		removed = true
		return nil
	})
	return ran && removed
}

// removeRecordFiles deletes the record's files; its .lock goes last, while the caller still holds it.
func removeRecordFiles(dir, name string) {
	id := strings.TrimSuffix(name, ".json")
	_ = os.Remove(filepath.Join(dir, name))
	_ = os.Remove(filepath.Join(dir, id+".log"))
	_ = os.Remove(filepath.Join(dir, id+paths.DispatchEventsSuffix))
	_ = os.Remove(AgyLogPath(id))
	_ = os.Remove(ranIdentityPath(id))
	_ = os.Remove(filepath.Join(dir, id+wire.LiveSockSuffix))
	_ = os.Remove(filepath.Join(dir, name+".lock"))
}
