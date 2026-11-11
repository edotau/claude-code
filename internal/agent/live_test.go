package agent

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

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// liveEnv gives the test a short /tmp config dir (a unix socket path caps at 104 bytes on macOS, 108 on Linux).
func liveEnv(t *testing.T) { pathstest.ShortConfigDir(t) }

type collect struct {
	mu  sync.Mutex
	got []wire.Event
}

func (c *collect) Emit(ev wire.Event) error {
	c.mu.Lock()
	c.got = append(c.got, ev)
	c.mu.Unlock()
	return nil
}

// An agent without a live mode is refused before anything starts.
func TestServeLiveRejectsAgentWithoutLive(t *testing.T) {
	liveEnv(t)
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	rec.Live = true
	if err := ServeLive(context.Background(), &rec); err == nil || !strings.Contains(err.Error(), "no live mode") {
		t.Errorf("ServeLive(codex) = %v, want a no-live-mode error", err)
	}
}

// A fifth session is refused; the cap counts only live, running records.
func TestLiveCapOfFour(t *testing.T) {
	liveEnv(t)
	for range MaxLiveSessions {
		rec := ClaimRecord("claude", RunOptions{}, time.Now())
		rec.Live, rec.PID = true, os.Getpid()
		_ = rec.Save()
	}
	if err := CheckLiveCap(); err == nil {
		t.Fatal("cap must refuse a fifth session")
	}
	dead := ClaimRecord("claude", RunOptions{}, time.Now())
	dead.Live, dead.Status = true, "ok"
	_ = dead.Save()
	if n, err := liveCount(); err != nil || n != MaxLiveSessions {
		t.Errorf("liveCount = %d, %v, want %d (finished sessions do not count)", n, err, MaxLiveSessions)
	}
}

// With three slots held, two contenders racing for the last one: exactly one wins, the loser is refused.
func TestLiveSlotLastSlotGoesToExactlyOne(t *testing.T) {
	liveEnv(t)
	for range MaxLiveSessions - 1 {
		if _, err := AcquireLiveSlot(); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() { <-start; _, err := AcquireLiveSlot(); errs <- err }()
	}
	close(start)
	won := 0
	for range 2 {
		if err := <-errs; err == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d contenders took the last slot, want exactly 1", won)
	}
}

// A released slot is reusable; a held one is not.
func TestLiveSlotReleaseFreesIt(t *testing.T) {
	liveEnv(t)
	var rel []func()
	for range MaxLiveSessions {
		r, err := AcquireLiveSlot()
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	if _, err := AcquireLiveSlot(); err == nil {
		t.Fatal("a fifth slot must be refused")
	}
	rel[0]()
	if _, err := AcquireLiveSlot(); err != nil {
		t.Errorf("released slot not reusable: %v", err)
	}
}

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

type noLauncher struct{ Launcher }

func TestRunHolderRestoresLauncher(t *testing.T) {
	setConfigDir(t)
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
	setConfigDir(t)
	const secret = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	orig := serveLive
	serveLive = func(context.Context, *Record) error { return fmt.Errorf("claude exited: token %s", secret) }
	t.Cleanup(func() { serveLive = orig })

	rec := ClaimRecord("claude", RunOptions{}, time.Now().Add(-2*time.Second))
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

func TestLiveFinishReturnsSessionFailure(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("claude", RunOptions{}, time.Now())
	s := liveSession{rec: &rec, name: "claude", profile: "test", session: "session-1"}
	want := errors.New("live child stream broke")
	if err := s.finish(want); !errors.Is(err, want) {
		t.Fatalf("finish = %v, want session failure %v", err, want)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error != want.Error() || got.SessionID != "session-1" {
		t.Fatalf("saved live failure = %+v", got)
	}
}

func TestLiveStartFailureDoesNotPublishSocket(t *testing.T) {
	liveEnv(t)
	rec := ClaimRecord("claude", RunOptions{}, time.Now())
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
