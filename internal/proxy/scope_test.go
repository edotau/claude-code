package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScopeCloseRemovesFileAndLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sc := Scope{Team: "t", ID: "i", PID: os.Getpid(), Start: "x", Port: 1, Started: time.Now()}
	cleanup, err := WriteScope(sc)
	if err != nil {
		t.Fatalf("WriteScope: %v", err)
	}
	file := scopeFile("t", "i")
	if _, err := os.Stat(file + ".lock"); err != nil {
		t.Fatalf("open scope has no lock file: %v", err)
	}
	cleanup()
	for _, p := range []string{file, file + ".lock"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("after close, %s still exists (stat err %v)", filepath.Base(p), err)
		}
	}
}
