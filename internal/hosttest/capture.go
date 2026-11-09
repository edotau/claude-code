package hosttest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// CaptureOutput redirects os.Stdout/os.Stderr around fn and returns fn's exit code plus both streams.
func CaptureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var bo, be bytes.Buffer
	var drained sync.WaitGroup
	drained.Go(func() { _, _ = io.Copy(&bo, ro) }) // drain while fn runs: a full pipe would block its writes forever
	drained.Go(func() { _, _ = io.Copy(&be, re) })
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	defer func() { // runs even when fn exits via t.Fatal/t.SkipNow (runtime.Goexit), so streams never stay redirected
		_ = wo.Close()
		_ = we.Close()
		os.Stdout, os.Stderr = oldOut, oldErr
		drained.Wait()
		_ = ro.Close()
		_ = re.Close()
		stdout, stderr = bo.String(), be.String()
	}()
	code = fn()
	return
}

// ClosedStdout points os.Stdout at a closed file so every write fails, until the test ends.
func ClosedStdout(t *testing.T) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-stdout"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = old })
}
