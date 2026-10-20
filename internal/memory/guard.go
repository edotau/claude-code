package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
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

type guardJournal struct {
	Phase    string `json:"phase"`
	Manifest string `json:"manifest"`
}

type guardJournalEntry struct {
	info    os.FileInfo
	journal guardJournal
}

// Cache decoded fields, not directory membership or readiness; migration writes must be visible immediately.
var guardJournalCache = struct {
	sync.Mutex
	entries map[string]guardJournalEntry
}{entries: make(map[string]guardJournalEntry)}

func readGuardJournal(path string) (guardJournal, error) {
	guardJournalCache.Lock()
	defer guardJournalCache.Unlock()

	key, keyErr := filepath.Abs(path)
	info, statErr := os.Stat(path)
	if keyErr == nil && statErr == nil {
		if cached, ok := guardJournalCache.entries[key]; ok &&
			os.SameFile(info, cached.info) &&
			info.Size() == cached.info.Size() &&
			info.ModTime().Equal(cached.info.ModTime()) &&
			info.Mode() == cached.info.Mode() {
			return cached.journal, nil
		}
	}
	delete(guardJournalCache.entries, key)
	var journal guardJournal
	data, err := os.ReadFile(path)
	if err != nil {
		return journal, err
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return journal, err
	}
	if keyErr == nil && statErr == nil {
		guardJournalCache.entries[key] = guardJournalEntry{info: info, journal: journal}
	}
	return journal, nil
}

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
		return Guard{Paused: true, Notice: "memory migration incomplete (" + filepath.Base(path) + "): finish it with `claude-code memory migrate --manifest " +
			manifest + "`; memory writes are paused"}
	}
	// After a completed import every resolver points into the store, so writes resume — unless the store vanished.
	if migrated && !StoreRepo().Exists() {
		return Guard{Paused: true, Migrated: true, Notice: "memory migration complete (" + filepath.Base(path) + ") but the store " +
			StoreDir() + " has no git repo; restore it before memory writes resume"}
	}
	return Guard{Migrated: migrated}
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
		j, err := readGuardJournal(p)
		if err == nil && j.Phase == journalComplete {
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
