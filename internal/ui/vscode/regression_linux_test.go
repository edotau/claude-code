package vscode

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinkExtensionPropagatesChtimesError(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "vscode", "claude-code-agents")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	// procfs permits Stat but rejects Chtimes, even when the test runs as root.
	if err := os.Symlink("/proc/self", source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	_, err := LinkExtension(home, t.TempDir(), false)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("LinkExtension must propagate the Chtimes *os.PathError; got %v", err)
	}
	if pathErr.Op != "chtimes" || pathErr.Path != source {
		t.Fatalf("wrong filesystem failure: %+v", pathErr)
	}
}

func TestWriteGuardedRejectsSaveAfterBackup(t *testing.T) {
	hermeticVSCode(t)
	dir := t.TempDir()
	dest, second := filepath.Join(dir, "settings.json"), filepath.Join(dir, "second.fifo")
	regressionMakeFIFO(t, dest)
	regressionMakeFIFO(t, second)
	const original = `{"local":1}`
	const saved = `{"local":2}`
	newFile := filepath.Join(dir, "saved.json")
	writeFile(t, newFile, saved)
	finished := make(chan regressionGuardedResult, 1)
	go func() {
		result, err := writeGuarded(dest, "settings", []byte(original), []byte(`{"merged":true}`))
		finished <- regressionGuardedResult{result, err}
	}()
	firstWriter := regressionFIFOReader(t, dest, finished)
	if _, err := firstWriter.WriteString(original); err != nil {
		t.Fatal(err)
	}
	// Replace the pathname, not the first reader's inode, before delivering EOF.
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(second, dest); err != nil {
		t.Fatal(err)
	}
	if err := firstWriter.Close(); err != nil {
		t.Fatal(err)
	}
	// This open succeeds only when the second check reads, after the backup.
	secondWriter := regressionFIFOReader(t, second, finished)
	if err := os.Rename(newFile, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := secondWriter.WriteString(saved); err != nil {
		t.Fatal(err)
	}
	if err := secondWriter.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-finished
	regressionAssertStaleSave(t, dest, saved, got)
}

type regressionGuardedResult struct {
	result Result
	err    error
}

func regressionMakeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A nonblocking writer open is a reader handshake, not a timing assumption.
// Keeping the two FIFO inodes distinct prevents the second writer from joining
// the first read. Completion without a reader catches the missing-check mutant.
func regressionFIFOReader(t *testing.T, path string, finished <-chan regressionGuardedResult) *os.File {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case got := <-finished:
			t.Fatalf("writeGuarded completed without the required freshness read: %+v, err %v", got.result, got.err)
		case <-timeout.C:
			t.Fatal("timed out waiting for a freshness reader")
		default:
		}
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err == nil {
			file := os.NewFile(uintptr(fd), path)
			t.Cleanup(func() { _ = file.Close() })
			return file
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		runtime.Gosched()
	}
}

func regressionAssertStaleSave(t *testing.T, dest, saved string, got regressionGuardedResult) {
	t.Helper()
	if got.err == nil || !strings.Contains(got.err.Error(), "changed under the merge") {
		t.Fatalf("want stale-merge error after backup; got %+v, err %v", got.result, got.err)
	}
	if got.result.Changed {
		t.Fatal("stale merge reported a change")
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != saved {
		t.Fatalf("concurrent save overwritten: got %s, want %s", body, saved)
	}
}
