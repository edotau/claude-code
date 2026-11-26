package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	syncEnv        = "NETWORK_MEMORY"
	syncEvery      = 30 * time.Minute
	syncStampName  = "memory-sync"
	SyncOffMsg     = "memory sync is off on this machine: claude-code vault set env NETWORK_MEMORY on"
	SyncCallLimit  = 2 * time.Minute
	syncPassLimit  = 10 * time.Minute
	syncRounds     = 20 // capped exports a pull follows before leaving the rest to the next pass
	peerAnswerMax  = 1 << 20
	ImportBusyExit = 75  // sysexits EX_TEMPFAIL: the store is busy, retry later; distinct from 1, 2 and ssh's 255
	sshFailExit    = 255 // ssh's own failure: no connection, no login
	sshTailMax     = 2048
)

// remoteHosts is the peer list seam; tests replace it.
var remoteHosts = providers.RemoteHosts

// SyncOn reports the opt-in NETWORK_MEMORY=on (process env, then the vault's); never in a sandbox.
func SyncOn() bool {
	if InSandbox() {
		return false
	}
	v := strings.TrimSpace(os.Getenv(syncEnv))
	if v == "" {
		v = strings.TrimSpace(providers.CustomEnv()[syncEnv])
	}
	return strings.EqualFold(v, "on")
}

// SyncIfDue starts the detached sync pass when opted in, the 30-minute slot is free and there was activity.
func SyncIfDue(now, active time.Time, cwd string) bool {
	return SyncOn() && syncIfDue(now, active, cwd)
}

// syncIfDue is SyncIfDue past the opt-in; the idle watcher reads the opt-in once and calls it every poll.
func syncIfDue(now, active time.Time, cwd string) bool {
	if fi, err := os.Stat(filepath.Join(paths.StateDir(), "preflight", syncStampName)); err == nil {
		if now.Sub(fi.ModTime()) < syncEvery || !active.After(fi.ModTime()) {
			return false
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if !catalog.ClaimEvery(syncStampName, syncEvery, now) || len(remoteHosts()) == 0 {
		return false
	}
	return spawn.sync(cwd)
}

// syncSSH runs script on host with a 5 s connect timeout; tail is ssh's stderr, exit 255 means ssh itself failed.
var syncSSH = func(ctx context.Context, host, script string, in io.Reader, out io.Writer) (string, int, error) {
	argv, err := syncArgv(host, script)
	if err != nil {
		return "", 0, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.TreeCancel(cmd, 0)
	cmd.WaitDelay = proc.ExecWaitDelay
	var stderr tailBuffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if body, ok := in.(*lazyBody); ok {
		err = runPushSSH(cmd, body)
	} else {
		cmd.Stdin = in
		err = cmd.Run()
	}
	tail := string(stderr.buf)
	if ctx.Err() != nil {
		return tail, 0, ctx.Err()
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return tail, exit.ExitCode(), nil
	}
	return tail, 0, err
}

type tailBuffer struct{ buf []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= sshTailMax {
		b.buf = append(b.buf[:0], p[n-sshTailMax:]...)
		return n, nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > sshTailMax {
		b.buf = b.buf[len(b.buf)-sshTailMax:]
	}
	return n, nil
}

// runPushSSH owns stdin copying, so a peer exiting before its first line can release the waiting reader.
func runPushSSH(cmd *exec.Cmd, body *lazyBody) error {
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(pipe, body)
		_ = pipe.Close()
		copied <- err
	}()
	err = cmd.Wait()
	close(body.stopped)
	if copyErr := <-copied; err == nil && copyErr != nil {
		err = copyErr
	}
	return err
}

// syncArgv is the streaming ssh argv with a 5 s connect timeout, so a host that drops packets fails fast.
func syncArgv(host, script string) ([]string, error) {
	argv, err := remote.StreamArgs(host, script)
	if err != nil {
		return nil, err
	}
	return slices.Insert(argv, 1, "-o", "ConnectTimeout=5"), nil
}

// StubSyncSSH replaces the ssh transport for tests; restore undoes it.
func StubSyncSSH(f func(ctx context.Context, host, script string, in io.Reader, out io.Writer) (string, int, error)) (restore func()) {
	prev := syncSSH
	syncSSH = f
	return func() { syncSSH = prev }
}

// serveScript is the fixed peer command for one serve mode; since was validated, so it carries no shell syntax.
func serveScript(mode string, since map[string]int64) string {
	s := remote.PathPrefix + " " + textutil.QuotePath(remote.HomePath(remote.Bin())) + " memory sync --serve " + mode
	if f := FormatSince(since); f != "" {
		s += " --since '" + f + "'"
	}
	return s
}

// RunSync is one pass under the try-once sync lock: pull from each peer, then push to each.
func RunSync(peers []string, out io.Writer) (int, error) {
	for _, p := range peers {
		if err := textutil.ValidHost(p); err != nil {
			fmt.Fprintf(out, "memory sync: %v; nothing was sent\n", err)
			return 2, nil
		}
	}
	if OpenEnvelopeStore == nil {
		return 0, errors.New("memory sync: no envelope store is linked")
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return 0, err
	}
	code := 0
	ran, err := paths.TryFileLock(filepath.Join(paths.MemoryStateDir(), "sync.lock"), 0, func() error {
		st, err := OpenEnvelopeStore()
		if err != nil {
			return err
		}
		defer st.Close()
		code = SyncPeers(context.Background(), st, peers, out)
		return nil
	})
	switch {
	case err != nil:
		return 0, err
	case !ran:
		fmt.Fprintln(out, "memory sync: another pass is running")
	}
	return code, nil
}

type syncResult int

const (
	syncOK      syncResult = iota
	syncDown               // no connection or login: retried at the next pass
	syncBusy               // the peer's or our bank lock was held: retried at the next pass
	syncRefused            // the peer or its stream was refused whole
	syncPending            // the pass limit ended while valid envelopes still remain
	syncHeld               // a pull held a gap or schema while other origins can still push
)

// SyncPeers pulls from every peer, then pushes to every one, and prints a summary; exit 1 when any did not converge.
func SyncPeers(ctx context.Context, st EnvelopeStore, peers []string, out io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, syncPassLimit)
	defer cancel()
	var count [6]int
	failed := map[string]syncResult{}
	for _, peer := range peers {
		if r := pullPeer(ctx, st, peer, out); r != syncOK {
			failed[peer] = r
		}
	}
	for _, peer := range peers {
		if failed[peer] != syncOK && failed[peer] != syncHeld {
			continue
		}
		if r := pushPeer(ctx, st, peer, out); r != syncOK {
			failed[peer] = r
		}
	}
	for _, r := range failed {
		count[r]++
	}
	fmt.Fprintf(out, "memory sync: %d peer(s): %d synced, %d unreachable, %d refused, %d busy, %d pending, %d held (retried next pass)\n",
		len(peers), len(peers)-len(failed), count[syncDown], count[syncRefused], count[syncBusy], count[syncPending], count[syncHeld])
	return min(len(failed), 1)
}

// sshResult files an ssh outcome: ok when the peer exited 0, else why in out.
func sshResult(out io.Writer, dir, peer, tail string, exit int, err error) syncResult {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		fmt.Fprintf(out, "%s %s: timed out; retried at the next pass\n", dir, peer)
		return syncPending
	case err != nil || exit == sshFailExit || exit < 0:
		fmt.Fprintf(out, "%s %s: unreachable: %s\n", dir, peer, strings.TrimSpace(cmpTail(err, tail)))
		return syncDown
	case exit == ImportBusyExit:
		fmt.Fprintf(out, "%s %s: busy; retried at the next pass\n", dir, peer)
		return syncBusy
	case exit != 0:
		fmt.Fprintf(out, "%s %s: refused (exit %d): %s\n", dir, peer, exit, lastLine(tail))
		return syncRefused
	}
	return syncOK
}

func cmpTail(err error, tail string) string {
	if err != nil {
		return err.Error()
	}
	return lastLine(tail)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// pullPeer asks the peer for everything past our watermarks and applies the stream in one transaction.
func pullPeer(ctx context.Context, st EnvelopeStore, peer string, out io.Writer) syncResult {
	var total ImportReport
	skip := map[string]int64{}
	result := syncOK
	for range syncRounds {
		since, err := Watermarks(st)
		if err != nil {
			fmt.Fprintf(out, "pull %s: refused: %v\n", peer, err)
			return syncRefused
		}
		maps.Copy(since, skip)
		cctx, cancel := context.WithTimeout(ctx, SyncCallLimit)
		buf := &limitedBuffer{cancel: cancel}
		tail, exit, err := syncSSH(cctx, peer, serveScript("export", since), nil, buf)
		cancel()
		if buf.over {
			fmt.Fprintf(out, "pull %s: stream is over the size cap; refused whole\n", peer)
			return syncRefused
		}
		if r := sshResult(out, "pull", peer, tail, exit, err); r != syncOK {
			return r
		}
		rep, n, r := applyStream(st, &buf.Buffer, out, peer)
		total.Applied, total.Dup, total.Conflicts = total.Applied+rep.Applied, total.Dup+rep.Dup, total.Conflicts+rep.Conflicts
		if r == syncHeld {
			result = syncHeld
			for _, origin := range rep.HeldOrigins {
				skip[origin] = sinceAll
			}
		} else if r != syncOK {
			return r
		}
		if n == 0 {
			fmt.Fprintf(out, "pull %s: %d applied, %d duplicate, %d conflicts\n", peer, total.Applied, total.Dup, total.Conflicts)
			return result
		}
		if rep.Applied == 0 && rep.Dup == 0 && rep.Conflicts == 0 && r != syncHeld {
			fmt.Fprintf(out, "pull %s: no progress in a nonempty stream; refused\n", peer)
			return syncRefused
		}
	}
	fmt.Fprintf(out, "pull %s: more envelopes remain; retried at the next pass\n", peer)
	return syncPending
}

// applyStream checks the whole stream, then imports it; a cut or miscounted stream applies nothing.
func applyStream(st EnvelopeStore, r io.Reader, out io.Writer, peer string) (ImportReport, int, syncResult) {
	body, n, err := ReadStream(r)
	if err != nil {
		fmt.Fprintf(out, "pull %s: refused: %v\n", peer, err)
		return ImportReport{}, 0, syncRefused
	}
	rep, err := ImportStream(st, bytes.NewReader(body))
	for _, h := range rep.Held {
		fmt.Fprintf(out, "pull %s: held: %s\n", peer, h)
	}
	switch {
	case errors.Is(err, ErrBankBusy):
		fmt.Fprintf(out, "pull %s: local bank busy; retried at the next pass\n", peer)
		return rep, n, syncBusy
	case err != nil:
		fmt.Fprintf(out, "pull %s: refused: %v\n", peer, err)
		return rep, n, syncRefused
	case len(rep.Held) > 0:
		return rep, n, syncHeld
	}
	return rep, n, syncOK
}

// firstLine hands the first output line to f and keeps everything after it.
type firstLine struct {
	f      func(line []byte)
	buf    bytes.Buffer
	done   bool
	cancel func()
	over   bool
}

func (w *firstLine) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > peerAnswerMax {
		w.over = true
		w.cancel()
		return 0, errors.New("memory sync: peer answer is over the size cap")
	}
	w.buf.Write(p)
	if i := bytes.IndexByte(w.buf.Bytes(), '\n'); i >= 0 && !w.done {
		w.done = true
		w.f(bytes.Clone(w.buf.Bytes()[:i]))
		w.buf.Next(i + 1)
	}
	return len(p), nil
}

type limitedBuffer struct {
	bytes.Buffer
	cancel func()
	over   bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > streamReadMax {
		b.over = true
		b.cancel()
		return 0, errors.New("memory sync: stream is over the size cap")
	}
	return b.Buffer.Write(p)
}

// lazyBody is the push's stdin: it waits for the peer's watermark line, then yields the stream built from it.
type lazyBody struct {
	ctx     context.Context
	ready   chan struct{}
	stopped chan struct{}
	once    sync.Once
	body    []byte
	skip    map[string]int64
	err     error
	r       *bytes.Reader
}

func (l *lazyBody) Read(p []byte) (int, error) {
	select {
	case <-l.ready:
	case <-l.stopped:
		return 0, io.EOF
	case <-l.ctx.Done():
		return 0, l.ctx.Err()
	}
	if l.err != nil {
		return 0, l.err
	}
	if l.r == nil {
		l.r = bytes.NewReader(l.body)
	}
	return l.r.Read(p)
}

// fill builds the stream past the peer's first line, once.
func (l *lazyBody) fill(st EnvelopeStore, line []byte) {
	l.once.Do(func() {
		defer close(l.ready)
		var got serveSince
		if l.err = decodeSince(line, &got); l.err != nil {
			return
		}
		maps.Copy(got.Since, l.skip)
		var b bytes.Buffer
		l.err = ServeExport(st, got.Since, &b)
		l.body = b.Bytes()
	})
}

func decodeSince(line []byte, got *serveSince) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return fmt.Errorf("memory sync: the peer's watermark line: %w", err)
	}
	if len(fields["since"]) == 0 || string(fields["since"]) == "null" {
		return errors.New("memory sync: the peer sent no watermarks")
	}
	if err := json.Unmarshal(fields["since"], &got.Since); err != nil {
		return fmt.Errorf("memory sync: the peer's watermarks: %w", err)
	}
	for origin, seq := range got.Since {
		if !originRE.MatchString(origin) || seq < 0 {
			return errors.New("memory sync: the peer sent an invalid watermark")
		}
	}
	return nil
}

// pushPeer repeats capped streams until the peer has every available envelope.
func pushPeer(ctx context.Context, st EnvelopeStore, peer string, out io.Writer) syncResult {
	var total serveApplied
	skip := map[string]int64{}
	held := false
	for range syncRounds {
		got, n, result := pushOnce(ctx, st, peer, out, skip)
		if result == syncHeld {
			held = true
			for _, origin := range got.HeldOrigins {
				skip[origin] = sinceAll
			}
		} else if result != syncOK {
			return result
		}
		total.Applied += got.Applied
		total.Dup += got.Dup
		total.Conflicts += got.Conflicts
		if result == syncHeld && len(got.HeldOrigins) == 0 {
			return syncHeld
		}
		if n > 0 && got.Applied == 0 && got.Dup == 0 && got.Conflicts == 0 && result != syncHeld {
			fmt.Fprintf(out, "push %s: no progress in a nonempty stream; refused\n", peer)
			return syncRefused
		}
		if n == 0 {
			fmt.Fprintf(out, "push %s: %d applied, %d duplicate, %d conflicts\n", peer, total.Applied, total.Dup, total.Conflicts)
			if held {
				return syncHeld
			}
			return syncOK
		}
	}
	fmt.Fprintf(out, "push %s: more envelopes remain; retried at the next pass\n", peer)
	return syncPending
}

// pushOnce reads the peer's watermarks and sends one size-capped stream.
func pushOnce(ctx context.Context, st EnvelopeStore, peer string, out io.Writer, skip map[string]int64) (serveApplied, int, syncResult) {
	cctx, cancel := context.WithTimeout(ctx, SyncCallLimit)
	defer cancel()
	in := &lazyBody{skip: skip, ctx: cctx, ready: make(chan struct{}), stopped: make(chan struct{})}
	ans := &firstLine{f: func(line []byte) { in.fill(st, line) }, cancel: cancel}
	tail, exit, err := syncSSH(cctx, peer, serveScript("import", nil), in, ans)
	if ans.over {
		fmt.Fprintf(out, "push %s: peer answer is over the size cap; refused\n", peer)
		return serveApplied{}, 0, syncRefused
	}
	var sum serveApplied
	if in.err != nil {
		fmt.Fprintf(out, "push %s: local export: %v\n", peer, in.err)
		return serveApplied{}, 0, syncRefused
	}
	if r := sshResult(out, "push", peer, tail, exit, err); r != syncOK {
		return serveApplied{}, 0, r
	}
	if decodeApplied(ans.buf.Bytes(), &sum) != nil {
		fmt.Fprintf(out, "push %s: invalid peer answer; refused\n", peer)
		return serveApplied{}, 0, syncRefused
	}
	_, n, err := ReadStream(bytes.NewReader(in.body))
	if err != nil {
		fmt.Fprintf(out, "push %s: local stream: %v\n", peer, err)
		return serveApplied{}, 0, syncRefused
	}
	if len(sum.Held) > 0 {
		fmt.Fprintf(out, "push %s: held: %s\n", peer, strings.Join(sum.Held, "; "))
		return sum, n, syncHeld
	}
	return sum, n, syncOK
}

func decodeApplied(line []byte, got *serveApplied) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &fields); err != nil {
		return err
	}
	for _, key := range []string{"applied", "dup", "conflicts"} {
		if len(fields[key]) == 0 || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return errors.New("memory sync: peer answer omitted counts")
		}
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), got); err != nil {
		return err
	}
	if got.Applied < 0 || got.Dup < 0 || got.Conflicts < 0 {
		return errors.New("memory sync: peer answer has negative counts")
	}
	return nil
}
