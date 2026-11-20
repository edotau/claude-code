package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

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

// liveGemini is the fixture gemini with a live mode whose Build records the request it was handed and stops there.
type liveGemini struct {
	fixtureGemini
	got *RunOptions
}

func (liveGemini) Available() error { return nil }

func (g liveGemini) NewLive() LiveBackend { return recordingLive{g.got} }

type recordingLive struct{ got *RunOptions }

var errBuildStop = errors.New("build reached")

func (b recordingLive) Build(_ context.Context, req RunOptions) (*exec.Cmd, *retry.TailBuffer, string, error) {
	*b.got = req
	return nil, nil, "", errBuildStop
}

func (recordingLive) WriteTurn(io.Writer, string) error { return nil }

func (recordingLive) Parse(*bufio.Reader, wire.Sink, func(kind, text string)) (wire.Result, error) {
	return wire.Result{}, nil
}

// A live read-only session gets the emulated read-only rules from checkRun, as the ask path does.
func TestServeLiveAppliesCheckRun(t *testing.T) {
	liveEnv(t)
	var got RunOptions
	registryMu.Lock()
	orig := drivers["gemini"]
	drivers["gemini"] = liveGemini{fixtureGemini{fixtureDriver{NewBaseEngine("gemini")}}, &got}
	registryMu.Unlock()
	t.Cleanup(func() { registryMu.Lock(); drivers["gemini"] = orig; registryMu.Unlock() })
	rec := ClaimRecord("gemini", RunOptions{Tools: true, ReadOnly: true}, time.Now())
	rec.Live = true
	if err := ServeLive(context.Background(), &rec); !errors.Is(err, errBuildStop) {
		t.Fatalf("ServeLive = %v, want the fake Build's stop", err)
	}
	if !slices.Contains(got.Permissions.Allow, permission.ReadOnlyAllow) || !slices.Contains(got.Permissions.Deny, permission.ReadOnlyDeny[0]) {
		t.Errorf("Build got permissions %+v, want the read-only rules checkRun adds", got.Permissions)
	}
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
	for range maxLiveSessions {
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
	if n, err := liveCount(); err != nil || n != maxLiveSessions {
		t.Errorf("liveCount = %d, %v, want %d (finished sessions do not count)", n, err, maxLiveSessions)
	}
}

// With three slots held, two contenders racing for the last one: exactly one wins, the loser is refused.
func TestLiveSlotLastSlotGoesToExactlyOne(t *testing.T) {
	liveEnv(t)
	for range maxLiveSessions - 1 {
		rel, err := acquireLiveSlot() // kept: a dropped release lets GC close the file and free the lock
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(rel)
	}
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			rel, err := acquireLiveSlot()
			if err == nil {
				t.Cleanup(rel)
			}
			errs <- err
		}()
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
	for range maxLiveSessions {
		r, err := acquireLiveSlot()
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	if _, err := acquireLiveSlot(); err == nil {
		t.Fatal("a fifth slot must be refused")
	}
	rel[0]()
	if _, err := acquireLiveSlot(); err != nil {
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

type panicLive struct{ recordingLive }

func (panicLive) WriteTurn(io.Writer, string) error { panic("turn boom") }

// A panic while serving one client reaches that client as an error line and never escapes serveConn.
func TestLiveServeConnContainsPanic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       func(cancel func(error)) *liveSession
		wantEnd bool
	}{
		{"before_turn", func(cancel func(error)) *liveSession { return &liveSession{name: "gemini", cancel: cancel} }, false},
		{"in_turn", func(cancel func(error)) *liveSession {
			return &liveSession{rec: &Record{ID: "r"}, name: "gemini", be: panicLive{}, stdin: io.Discard,
				events: &wire.EventLog{}, idle: time.NewTimer(time.Hour), idleFor: time.Hour, cancel: cancel}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			s := tc.s(cancel)
			srv, cli := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); defer srv.Close(); s.serveConn(ctx, srv) }()
			go func() { _, _ = cli.Write([]byte(`{"schema":1,"prompt":"hi"}` + "\n")) }() // a pipe has no buffer
			var last string
			for sc := bufio.NewScanner(cli); sc.Scan(); {
				last = sc.Text()
			}
			<-done
			if !strings.Contains(last, `"status":"refused"`) || strings.Contains(last, "boom") {
				t.Errorf("last client line = %q, want a generic refused error", last)
			}
			if got := ctx.Err() != nil; got != tc.wantEnd {
				t.Errorf("session ended = %v, want %v", got, tc.wantEnd)
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
	const secret = "sk" + "-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
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

func TestLiveFinishKeepsTurnUsage(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("claude", RunOptions{}, time.Now())
	s := liveSession{rec: &rec, name: "claude", profile: "test", usage: wire.Usage{InputTokens: 7, OutputTokens: 3}}
	if err := s.finish(errLiveIdle); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.InputTokens != 7 || got.Usage.OutputTokens != 3 {
		t.Fatalf("saved usage = %+v, want the turns' 7 in / 3 out", got.Usage)
	}
}

func TestAddTurnUsageCostIsCumulative(t *testing.T) {
	sum := addTurnUsage(wire.Usage{}, wire.Usage{InputTokens: 2, OutputTokens: 3, CacheCreationTokens: 48678, CostUSD: 0.0097373})
	sum = addTurnUsage(sum, wire.Usage{InputTokens: 2, OutputTokens: 3, CacheReadTokens: 33433, CacheCreationTokens: 16945, CostUSD: 0.01346233})
	want := wire.Usage{InputTokens: 4, OutputTokens: 6, CacheReadTokens: 33433, CacheCreationTokens: 65623, CostUSD: 0.01346233}
	if sum != want {
		t.Fatalf("usage = %+v, want %+v", sum, want)
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

// A live turn's ran-on line names the Databricks workspace it ran on; the agent's own login still reads "own login".
func TestLiveRanOnNamesWorkspace(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", pinDbxRow)
	for _, tc := range []struct{ profile, want string }{
		{"dev", "ran on gemini · databricks·dev · "},
		{SchemaFor("gemini").OwnProfile(), "ran on gemini · own login · "},
	} {
		s := &liveSession{name: "gemini", profile: tc.profile, account: liveAccount("gemini", tc.profile)}
		res := wire.Result{Agent: "gemini", Provider: tc.profile, Hops: s.hops(""), Status: wire.StatusOK, DurationMs: 1000}
		if got := wire.RanOn(res); !strings.HasPrefix(got, tc.want) {
			t.Errorf("profile %q: RanOn = %q, want prefix %q", tc.profile, got, tc.want)
		}
	}
}
