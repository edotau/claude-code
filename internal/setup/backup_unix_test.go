//go:build unix

package setup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCopyTreeSkipsFIFO(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "backup")
	mustWrite(t, filepath.Join(src, "nested", "z-file"), "preserved\n")
	fifo := filepath.Join(src, "nested", "a-pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	copyTreeWithFIFODeadline(t, src, dst, fifo)
	if _, err := os.Lstat(filepath.Join(dst, "nested", "a-pipe")); !os.IsNotExist(err) {
		t.Fatalf("FIFO was not skipped: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "nested", "z-file")); err != nil || string(got) != "preserved\n" {
		t.Fatalf("regular file = %q, %v; want preserved content", got, err)
	}
}

func copyTreeWithFIFODeadline(t *testing.T, src, dst, fifo string) {
	t.Helper()
	// Keep a writer open so a mistaken FIFO read blocks until the timeout releases it.
	keeper, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- copyTree(src, dst) }()
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		if err := keeper.Close(); err != nil {
			t.Fatal(err)
		}
		<-done
		t.Fatal("copyTree blocked reading a FIFO instead of skipping it")
	}
	if closeErr := keeper.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}
