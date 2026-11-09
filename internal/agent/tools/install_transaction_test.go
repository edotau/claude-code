package tools

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestCodexRollbackSerializesConcurrentRelease(t *testing.T) {
	setupToolsTestEnv(t)
	dir := paths.BinDir()
	host := "codex-code-mode-host"
	writeToolsFixture(t, filepath.Join(dir, host), "original host", 0o755)
	writeToolsFixture(t, filepath.Join(dir, "codex"), "original codex", 0o755)
	// The target fits NAME_MAX, but AtomicWrite's longer temporary basename forces the second write to fail.
	badName := "codex-" + strings.Repeat("x", 244)
	writeToolsFixture(t, filepath.Join(dir, badName), "original failed codex", 0o755)
	gate := &transactionGate{paused: make(chan struct{}), resume: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.resume) }) }
	t.Cleanup(unblock)
	failed := startInstallTransaction(gate, dir, []stagedBinary{
		{name: host, bin: []byte("failed release host")},
		{name: badName, bin: []byte("failed release codex")},
	})
	select {
	case <-gate.paused:
	case <-time.After(5 * time.Second):
		t.Fatal("failing install did not reach its second write")
	}
	succeeded := startInstallTransaction(io.Discard, dir, []stagedBinary{
		{name: host, bin: []byte("successful release host")},
		{name: "codex", bin: []byte("successful release codex")},
	})
	select {
	case err := <-succeeded:
		succeeded <- err
		t.Error("concurrent release completed before the failing transaction released its lock")
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	if err := transactionResult(t, failed); err == nil || !strings.Contains(err.Error(), badName) {
		t.Fatalf("second write error = %v; want failure for %s", err, badName)
	}
	if err := transactionResult(t, succeeded); err != nil {
		t.Fatal(err)
	}
	assertInstalledCodexBinary(t, host, "successful release host")
	assertInstalledCodexBinary(t, "codex", "successful release codex")
}

func startInstallTransaction(w io.Writer, dir string, all []stagedBinary) chan error {
	done, started := make(chan error, 1), make(chan struct{})
	go func() {
		close(started)
		done <- installBinaries(w, dir, all)
	}()
	<-started
	return done
}

type transactionGate struct {
	paused chan struct{}
	resume chan struct{}
	writes int
}

func (g *transactionGate) Write(p []byte) (int, error) {
	g.writes++
	if g.writes == 2 {
		close(g.paused)
		<-g.resume
	}
	return len(p), nil
}

func transactionResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("install transaction did not finish")
		return nil
	}
}
