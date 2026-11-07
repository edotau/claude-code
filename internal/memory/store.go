// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/memory/<repo>/ (harness itself memory/claude-code/), home bank in memory/home/.
package memory

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/edotau/claude-code/internal/memory/bank"

	"github.com/edotau/claude-code/internal/memstore"
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
		if err := seedFile(p, body); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// writeBody writes a seed file's body; tests swap it to fail mid-write.
var writeBody = func(f *os.File, body string) error { _, err := f.WriteString(body); return err }

// seedFile writes body to a temp file and links it into place, so p is never partial and never replaces a racer's file.
func seedFile(p, body string) error {
	f, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // the temp name is spare once linked, garbage if the write failed
	werr := writeBody(f, body)
	if err := errors.Join(werr, f.Chmod(0o600), f.Close()); err != nil {
		return err
	}
	err = os.Link(tmp, p)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return nil
	}
	return createExcl(p, body) // no hard links here (FUSE, network mounts)
}

// createExcl writes body to a new p, never replacing a racing writer's file.
func createExcl(p, body string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, werr := f.WriteString(body)
	return errors.Join(werr, f.Close())
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
