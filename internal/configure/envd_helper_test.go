package configure

import (
	"os"
	"path/filepath"
	"testing"
)

// mkEnvD creates dir/env.d (where providers.json lives) and returns it.
func mkEnvD(t *testing.T, dir string) string {
	t.Helper()
	d := filepath.Join(dir, "env.d")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}
