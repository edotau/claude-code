package job

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
)

func TestCancelDetachedSplitReview(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := executable
	executable = func() (string, error) { return sleeperScript(t), nil }
	t.Cleanup(func() { executable = old })
	rec := ClaimRecord("codex", agent.RunOptions{}, time.Now())
	rec.Progress = "part 1/2"
	if err := SpawnArgv(&rec, []string{"codex", "review", "--background"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kill(rec.PID) })
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := CancelJob(context.Background(), "codex", "", rec.ID)
	if err != nil {
		t.Fatalf("cancel detached split review: %v", err)
	}
	if got.Alive() || got.Status != "error" || !strings.Contains(got.Error, "cancelled") {
		t.Fatalf("cancelled review = %+v, want stopped and recorded cancellation", got)
	}
	assertSavedCancellation(t, rec.ID)
}

type acceptErrorListener struct {
	errs  []error
	calls int
}

func (ln *acceptErrorListener) Accept() (net.Conn, error) {
	ln.calls++
	if len(ln.errs) == 0 {
		return nil, net.ErrClosed
	}
	err := ln.errs[0]
	ln.errs = ln.errs[1:]
	return nil, err
}

func (*acceptErrorListener) Close() error   { return nil }
func (*acceptErrorListener) Addr() net.Addr { return nil }

func TestLiveAcceptRetriesAndReportsFailure(t *testing.T) {
	fatal := errors.New("listener failed")
	for _, tc := range []struct {
		name  string
		errs  []error
		calls int
		cause error
	}{
		{"resource_pressure", []error{fmt.Errorf("accept: %w", syscall.EMFILE), fatal}, 2, fatal},
		{"fatal", []error{fatal}, 1, fatal},
		{"closed", []error{net.ErrClosed}, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ln := &acceptErrorListener{errs: tc.errs}
			s := liveSession{cancel: cancel}
			var wg sync.WaitGroup
			s.accept(ctx, ln, &wg)
			if ln.calls != tc.calls {
				t.Fatalf("Accept calls = %d, want %d after transient failure", ln.calls, tc.calls)
			}
			if !errors.Is(context.Cause(ctx), tc.cause) {
				t.Fatalf("holder cause = %v, want %v", context.Cause(ctx), tc.cause)
			}
		})
	}
}

func TestLiveStartFailureDoesNotPublishSocket(t *testing.T) {
	liveEnv(t)
	rec := ClaimRecord("claude", agent.RunOptions{}, time.Now())
	sock := LiveSocket(rec.ID)
	const marker = "socket must not be touched before child starts"
	if err := os.WriteFile(sock, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cmd := exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-agent"))
	err := serveChild(ctx, cancel, &rec, "claude", nil, cmd, &retry.TailBuffer{}, "")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serveChild = %v, want failed child start", err)
	}
	got, err := os.ReadFile(sock)
	if err != nil || string(got) != marker {
		t.Fatalf("failed start touched socket path: content=%q err=%v", got, err)
	}
}

// Given a record that cannot be saved, When the live child starts, Then serveChild fails fast instead of serving.

// A nonempty directory at the record path makes the atomic rename fail even as root.

// Given an unreadable dispatch dir, Then the live cap and LoadRecord fail with the read error, not "0 records".

// A file where the dir belongs makes ReadDir fail even as root.

type terminalErrorReader struct{ err error }

func (r terminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestPumpReturnsReadErrorAfterDraining(t *testing.T) {
	want := errors.New("stdout read failed")
	const output = "buffered stdout"
	pump := newPumpBuf(io.MultiReader(strings.NewReader(output), terminalErrorReader{want}))
	select {
	case <-pump.done:
	case <-time.After(time.Second):
		t.Fatal("stdout pump did not finish")
	}
	got, err := io.ReadAll(pump)
	if string(got) != output || !errors.Is(err, want) {
		t.Fatalf("drained stdout = %q, %v; want %q then %v", got, err, output, want)
	}
}

// Given a holder that finished ok without ever opening its socket, Then WaitLiveReady fails with a non-empty message.
