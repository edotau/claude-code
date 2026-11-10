package job

import (
	"context"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A socket path over the unix limit fails with a message naming the length, not a bare "bind: invalid argument".
func TestListenLiveRefusesOverlongSocketPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), strings.Repeat("d", 100)))
	if _, err := listenLive("20261016T210512.123-claude"); err == nil || !strings.Contains(err.Error(), "unix limit") {
		t.Fatalf("listenLive = %v, want an over-the-unix-limit error", err)
	}
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

func (*acceptErrorListener) Close() error { return nil }

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

type noLauncher struct{ agent.Launcher }

func TestRunHolderRestoresLauncher(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var got *Record
	orig := serveLive
	serveLive = func(_ context.Context, rec *Record) error { got = rec; return nil }
	t.Cleanup(func() { serveLive = orig })

	rec := &Record{ID: "live-holder-test"}
	if code := RunHolder(rec, noLauncher{}); code != 0 {
		t.Fatalf("RunHolder = %d, want 0", code)
	}
	if got == nil || got.Request.Launcher == nil {
		t.Fatal("ServeLive got a record with no Launcher (json:\"-\" field lost on reload)")
	}
}

// Given a holder that fails with a credential-shaped error, Then the saved record is redacted and carries a duration.
func TestRunHolderRedactsAndTimesFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const secret = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	orig := serveLive
	serveLive = func(context.Context, *Record) error { return fmt.Errorf("claude exited: token %s", secret) }
	t.Cleanup(func() { serveLive = orig })

	rec := ClaimRecord("claude", agent.RunOptions{}, time.Now().Add(-2*time.Second))
	log, err := os.OpenFile(rec.LogPath(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = log
	t.Cleanup(func() { os.Stderr = stderr; _ = log.Close() })
	if code := RunHolder(&rec, noLauncher{}); code != 1 {
		t.Fatalf("RunHolder = %d, want 1", code)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || strings.Contains(got.Error, secret) {
		t.Errorf("record = %q/%q, want error status with the secret redacted", got.Status, got.Error)
	}
	if got.DurationMs < 1000 {
		t.Errorf("DurationMs = %d, want the failed holder's elapsed time", got.DurationMs)
	}
	body, err := os.ReadFile(rec.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	want := "claude-code: ask --live: " + got.Error + "\n"
	if string(body) != want {
		t.Errorf("holder stderr = %q, want only the redacted error %q", body, want)
	}
}
