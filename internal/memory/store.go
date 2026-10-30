// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/memory/<repo>/ (harness itself memory/claude-code/), home bank in memory/home/.
package memory

import (
	"embed"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

//go:embed templates/*.md
var templatesFS embed.FS

// bankFiles are the six typed files a bank carries (order = injection order).
var bankFiles = []string{
	"projectContext.md", "activeContext.md", "progress.md",
	"decisionLog.md", "conventions.md", "sessionHistory.md",
}

// BankFiles exports the six typed filenames for callers outside this package (e.g. internal/history)
// that must watch them without duplicating the list.
func BankFiles() []string { return bankFiles }

// SessionHistoryMaxEntries rotates the oldest sessionHistory entries to <bank>/archive/.
const SessionHistoryMaxEntries = 30

// FileCaps are the per-file line caps `claude-code memory update` holds; sessionHistory rotates by entry count instead.
var FileCaps = map[string]int{
	"projectContext.md": 60,
	"activeContext.md":  80,
	"progress.md":       60,
	"decisionLog.md":    200,
	"conventions.md":    80,
}

// UpdateChildEnv marks a spawned update worker so its own session hooks never schedule another.
const UpdateChildEnv = "CLAUDE_SESSION_UPDATE_CHILD"

// IsUpdateChild reports whether this process runs inside an update worker.
func IsUpdateChild() bool {
	v := os.Getenv(UpdateChildEnv)
	return v != "" && v != "0"
}

// HomeBankDir is the shared bank: store/home.
func HomeBankDir() string { return homeStoreDir() }

// homeStoreDir is the home dir inside the store.
func homeStoreDir() string { return filepath.Join(StoreDir(), "home") }

// Dir is root's local bank; a non-repo root collapses to the home one.
func Dir(root string) string {
	if d, err := RepoDir(root); err == nil { // one dir per repo shared with native memory; the harness is memory/claude-code
		return d
	}
	return homeStoreDir()
}

// IsHomeBank reports whether root's local bank is the home bank (read it once, not twice).
func IsHomeBank(root string) bool { return paths.SamePath(Dir(root), HomeBankDir()) }

// HasBank reports whether any typed file exists in root's local bank.
func HasBank(root string) bool {
	for _, name := range bankFiles {
		if _, err := os.Stat(filepath.Join(Dir(root), name)); err == nil {
			return true
		}
	}
	return false
}

// Init ensures root's bank exists and seeds missing files (never overwrites); returns the bank dir.
func Init(root string) (string, error) {
	dir, err := EnsureRepoDir(root) // a new repo store needs its origin record before any file lands in it
	if err != nil {
		return "", err
	}
	for name, body := range templates() {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := paths.AtomicWrite(p, []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// IsProjectContextTemplate reports whether root's projectContext.md still equals the scaffold
// template — unfilled, so `memory init` should still offer its enrichment procedure.
func IsProjectContextTemplate(root string) bool {
	data, err := os.ReadFile(filepath.Join(Dir(root), "projectContext.md"))
	if err != nil {
		return false
	}
	return string(data) == templates()["projectContext.md"]
}

// GitToplevel is dir's git work-tree root, "" outside a repo.
func GitToplevel(dir string) string {
	out, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// templates seed a fresh bank's six files; `claude-code memory update` fills them from real session work.
var templates = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, len(bankFiles))
	for _, name := range bankFiles {
		body, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			panic(err) // embedded alongside bankFiles; a mismatch is a build-time bug, not a runtime one
		}
		m[name] = string(body)
	}
	return m
})
