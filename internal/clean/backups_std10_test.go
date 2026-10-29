package clean

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestStd10DefaultCleanKeepsNewestBackupMirror: a default sweep must leave the newest mirrored backup of a file.
func TestStd10DefaultCleanKeepsNewestBackupMirror(t *testing.T) {
	dir := vaulttest.Use(t)
	orig := filepath.Join(dir, "vendor", "config.toml")
	fresh := paths.BackupPath(orig, time.Now())
	stale := paths.BackupPath(orig, time.Now().Add(-400*24*time.Hour))
	old := time.Now().Add(-400 * 24 * time.Hour)
	write(t, stale, "old")
	write(t, fresh, "new")
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("Run = %d: %s", code, errw.String())
	}
	if !exists(t, fresh) {
		t.Fatalf("newest backup mirror %s was deleted by a default clean\n%s", fresh, errw.String())
	}
}
