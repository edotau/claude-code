package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Guard is the readiness verdict every hook/worker consults before initializing, spawning, stamping or writing.
type Guard struct {
	Notice   string
	Paused   bool // no write, spawn, stamp or scaffold until the notice's condition clears
	Migrated bool // a migration completed: the store is the home; paused until the cutover points the resolvers at it
}

// ErrMemoryPaused wraps every refusal from CheckWritable; errors.Is lets callers stay quiet instead of failing loud.
var ErrMemoryPaused = errors.New("memory writes paused")

// journalComplete is the phase a migration journal reaches only when its apply finished.
const journalComplete = "complete"

// StartupGuard pauses writes during maintenance, an unfinished migration or a missing migrated store; a pure read.
func StartupGuard() Guard {
	if reason, ok := InMaintenance(); ok {
		if reason == "" {
			reason = "no reason recorded"
		}
		return Guard{Paused: true, Notice: "memory maintenance in progress (" + reason + "): memory scaffolding and harvests are paused until it completes; see " + MaintenancePath()}
	}
	path, manifest, incomplete, migrated := scanJournals()
	if incomplete {
		return Guard{Paused: true, Notice: "memory migration incomplete (" + filepath.Base(path) + "): finish it with `claude-code memory migrate --apply " +
			manifest + "`; memory writes are paused"}
	}
	// Dir/GlobalDir/NativeDir still resolve the retired legacy bank until the Phase 2 cutover: unpausing here
	// would let SessionStart/harvests re-scaffold it, so a completed import stays paused with its own notice.
	if migrated {
		return Guard{Paused: true, Migrated: true, Notice: "memory migration complete (" + filepath.Base(path) + "): the legacy bank " +
			GlobalDir() + " is retired and the store at " + StoreDir() + " is not wired for writes yet; memory scaffolding and harvests stay paused"}
	}
	return Guard{}
}

// CheckWritable is the one mutation gate: nil when the store accepts writes, else ErrMemoryPaused with the notice.
func CheckWritable() error {
	if g := StartupGuard(); g.Paused {
		return fmt.Errorf("%w: %s", ErrMemoryPaused, g.Notice)
	}
	return nil
}

// WithWritableBankLock is WithBankLock for mutations; the recheck under the lock catches a marker raised meanwhile.
func WithWritableBankLock(wait time.Duration, fn func() error) error {
	if err := CheckWritable(); err != nil {
		return err
	}
	return WithBankLock(wait, func() error {
		if err := CheckWritable(); err != nil {
			return err
		}
		return fn()
	})
}

// scanJournals walks MigrateStateDir()/*.journal.json by name; the first not provably complete (or unreadable)
// returns incomplete, else path is the newest complete journal and migrated says one exists.
func scanJournals() (path, manifest string, incomplete, migrated bool) {
	matches, _ := filepath.Glob(filepath.Join(MigrateStateDir(), "*.journal.json"))
	sort.Strings(matches)
	for _, p := range matches {
		var j struct {
			Phase    string `json:"phase"`
			Manifest string `json:"manifest"`
		}
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &j) == nil && j.Phase == journalComplete {
			path, migrated = p, true
			continue
		}
		if j.Manifest == "" {
			j.Manifest = "<manifest path from " + p + ">"
		}
		return p, j.Manifest, true, migrated
	}
	return path, "", false, migrated
}
