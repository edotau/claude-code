package memory

import (
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Keep this test serial: the helper briefly changes the process-wide file-size limit.
func TestPersistenceCreateExclRemovesPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projectContext.md")
	if err := createExclWithFileLimit(t, path); err == nil {
		t.Fatal("expected a partial-write error")
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed fallback left a partial file: %v", err)
	}
	if err := createExcl(path, "retry\n"); err != nil {
		t.Fatal(err)
	}
	if got := aliasRead(t, path); got != "retry\n" {
		t.Errorf("retry retained partial scaffold: %q", got)
	}
}

func createExclWithFileLimit(t *testing.T, path string) error {
	t.Helper()
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	limit := previous
	limit.Cur = 16
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
			t.Fatal(err)
		}
	}()
	wasIgnored := signal.Ignored(syscall.SIGXFSZ)
	signal.Ignore(syscall.SIGXFSZ)
	defer func() {
		if !wasIgnored {
			signal.Reset(syscall.SIGXFSZ)
		}
	}()
	return createExcl(path, strings.Repeat("x", int(limit.Cur)+1))
}
