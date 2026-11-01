// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/memory/<repo>/ (harness itself memory/claude-code/), home bank in memory/home/.
package memory

import (
	"embed"
	"os"
	"path/filepath"
	"sync"

	"github.com/edotau/claude-code/internal/memory/bank"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

//go:embed templates/*.md
var templatesFS embed.FS

// FileCaps are the per-file line caps `claude-code memory update` holds; sessionHistory rotates by entry count instead.
var FileCaps = map[string]int{
	"projectContext.md": 60,
	"activeContext.md":  80,
	"progress.md":       60,
	"decisionLog.md":    200,
	"conventions.md":    80,
}

// HasBank reports whether any typed file exists in root's local bank.
func HasBank(root string) bool {
	for _, name := range bank.BankFiles() {
		if _, err := os.Stat(filepath.Join(memstore.Dir(root), name)); err == nil {
			return true
		}
	}
	return false
}

// Init ensures root's bank exists and seeds missing files (never overwrites); returns the bank dir.
func Init(root string) (string, error) {
	dir, err := memstore.EnsureRepoDir(root) // a new repo store needs its origin record before any file lands in it
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
	data, err := os.ReadFile(filepath.Join(memstore.Dir(root), "projectContext.md"))
	if err != nil {
		return false
	}
	return string(data) == templates()["projectContext.md"]
}

// templates seed a fresh bank's six files; `claude-code memory update` fills them from real session work.
var templates = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, len(bank.BankFiles()))
	for _, name := range bank.BankFiles() {
		body, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			panic(err) // embedded alongside bankFiles; a mismatch is a build-time bug, not a runtime one
		}
		m[name] = string(body)
	}
	return m
})
