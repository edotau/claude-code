package settings

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteBackupNeverTruncatesExisting checks a second backup under the same name fails and keeps the first.
func TestWriteBackupNeverTruncatesExisting(t *testing.T) {
	name := filepath.Join(t.TempDir(), "settings.json.bak.x")
	if err := writeBackup(name, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBackup(name, []byte("second"), 0o644); !os.IsExist(err) {
		t.Fatalf("second writeBackup err = %v, want exists", err)
	}
	if got, _ := os.ReadFile(name); string(got) != "first" {
		t.Errorf("backup = %q, want first", got)
	}
}
