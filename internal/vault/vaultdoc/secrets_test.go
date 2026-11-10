package vaultdoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRecordLabelsReportsUnreadableVault(t *testing.T) {
	dir := vaulttest.UseFreshKey(t)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "vault.db"), []byte("not a vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	var labels map[string]string
	_, _, stderr := hosttest.CaptureOutput(t, func() int { labels = recordLabels(); return 0 })
	if len(labels) != 0 || !strings.Contains(stderr, "vault ls: reading labels:") {
		t.Fatalf("labels=%v stderr=%q, want empty map and a reading-labels warning", labels, stderr)
	}
}
