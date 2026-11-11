package tui

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proc"
)

func TestDecodeBasicKeys(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want Key
		n    int
	}{
		{"enter-cr", []byte("\r"), Key{Kind: KeyEnter}, 1},
		{"enter-lf", []byte("\n"), Key{Kind: KeyEnter}, 1},
		{"ctrl-c", []byte{0x03}, Key{Kind: KeyCtrlC}, 1},
		{"esc-alone", []byte{0x1b}, Key{Kind: KeyEsc}, 1},
		{"up", []byte("\x1b[A"), Key{Kind: KeyUp}, 3},
		{"down", []byte("\x1b[B"), Key{Kind: KeyDown}, 3},
		{"left", []byte("\x1b[D"), Key{Kind: KeyLeft}, 3},
		{"rune-j", []byte("j"), Key{Kind: KeyRune, Rune: 'j'}, 1},
		{"rune-q", []byte("q"), Key{Kind: KeyRune, Rune: 'q'}, 1},
		{"rune-utf8", []byte("é"), Key{Kind: KeyRune, Rune: 'é'}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := decode(c.buf)
			if got != c.want || n != c.n {
				t.Errorf("Decode(%q) = %+v, %d; want %+v, %d", c.buf, got, n, c.want, c.n)
			}
		})
	}
}

func TestDecodeIncomplete(t *testing.T) {
	cases := [][]byte{
		{0x1b, '['}, // CSI with no final byte yet
		{0xc3},      // split UTF-8 rune (é is 0xc3 0xa9)
		nil,         // empty buffer
	}
	for _, buf := range cases {
		if _, n := decode(buf); n != 0 {
			t.Errorf("Decode(%v): n = %d, want 0 (need more input)", buf, n)
		}
	}
}

func TestDecodeDropsUnknown(t *testing.T) {
	cases := [][]byte{
		{0x1c},           // unbound control byte
		[]byte("\x1b[Z"), // CSI final byte with no mapping
	}
	for _, buf := range cases {
		key, n := decode(buf)
		if n == 0 {
			t.Fatalf("Decode(%v): n = 0, want it to consume and drop", buf)
		}
		if key != dropped {
			t.Errorf("Decode(%v) = %+v, want the dropped zero key", buf, key)
		}
	}
}

func TestDrainMultipleKeysAndCarriesPartialTail(t *testing.T) {
	keys, tail := drain([]byte("j\x1b[A\x1b["))
	want := []Key{{Kind: KeyRune, Rune: 'j'}, {Kind: KeyUp}}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Errorf("drain keys = %+v, want %+v", keys, want)
	}
	if string(tail) != "\x1b[" {
		t.Errorf("drain tail = %q, want the incomplete CSI carried over", tail)
	}
}

func TestDrainCarriesLoneEscIntoArrow(t *testing.T) {
	keys, tail := drain([]byte("\x1b"))
	if len(keys) != 0 || string(tail) != "\x1b" {
		t.Fatalf("first read: keys=%+v tail=%q; want no keys and pending Esc", keys, tail)
	}
	keys, tail = drain(append(tail, []byte("[A")...))
	if len(keys) != 1 || keys[0].Kind != KeyUp || len(tail) != 0 {
		t.Fatalf("second read: keys=%+v tail=%q; want Up and no tail", keys, tail)
	}
}

func TestDrainDropsUnknownBytes(t *testing.T) {
	keys, tail := drain([]byte{0x1c, 0x1c, 'k'})
	if len(keys) != 1 || keys[0].Kind != KeyRune || keys[0].Rune != 'k' {
		t.Errorf("drain = %+v, want only the trailing rune key", keys)
	}
	if len(tail) != 0 {
		t.Errorf("drain tail = %q, want none left pending", tail)
	}
}

// Open requires a real TTY on stdin; under `go test` stdin is never one, so this only proves the
// clean failure path. A raw-mode exercise of Open/Close runs manually via a pty, not in CI.
func TestOpenFailsCleanlyWithoutATTY(t *testing.T) {
	if proc.IsTerminalFD(int(os.Stdin.Fd())) {
		t.Skip("stdin is a TTY in this environment; the no-TTY path isn't exercised here")
	}
	tm, err := openTerminal()
	if err == nil {
		tm.Close()
		t.Fatal("Open() succeeded without a TTY on stdin")
	}
}

// TestDecodeRightArrow: ESC [ C is Right, not dropped.
func TestDecodeRightArrow(t *testing.T) {
	k, n := decode([]byte{0x1b, '[', 'C'})
	if k.Kind != KeyRight || n != 3 || k.Name() != "right" {
		t.Fatalf("Decode(ESC [ C) = %+v,%d", k, n)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func TestTerminalCloseReportsRestoreError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("restore is a no-op without termios")
	}
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	term := terminal{fd: -1, prev: &termState{}, out: out}
	if err := term.Close(); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("Close = %v; want terminal restore EBADF", err)
	}
}

func TestTerminalReportsInputEIO(t *testing.T) {
	master, slave := hosttest.PTY(t)
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	term := terminal{fd: int(master.Fd()), in: master, keys: make(chan Key, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go term.readKeys()
	t.Cleanup(term.stopReader)
	select {
	case _, ok := <-term.Keys():
		if ok {
			t.Fatal("input EIO emitted a key instead of closing the stream")
		}
	case <-time.After(time.Second):
		t.Fatal("input EIO did not close the key stream")
	}
	want := error(syscall.EIO)
	if runtime.GOOS == "darwin" {
		want = io.EOF // darwin's pty master reads EOF, not EIO, once the slave is gone
	}
	if err := term.Err(); !errors.Is(err, want) {
		t.Fatalf("Err after Keys closed = %v; want input %v", err, want)
	}
}

// interruptPolls makes every select longer than 10ms return EINTR after 10ms of silence; it returns the count.
func interruptPolls(t *testing.T) *atomic.Int32 {
	t.Helper()
	if !canPoll {
		t.Skip("no select(2) wait on this platform")
	}
	real, interrupts := pollOnce, new(atomic.Int32)
	pollOnce = func(fd int, d time.Duration) (bool, error) {
		const every = 10 * time.Millisecond
		if d <= every {
			return real(fd, d)
		}
		if readable, err := real(fd, every); readable || err != nil {
			return readable, err
		}
		interrupts.Add(1)
		return false, syscall.EINTR
	}
	t.Cleanup(func() { pollOnce = real })
	return interrupts
}

func TestWaitReadableDeadlineSurvivesSignals(t *testing.T) {
	interrupts := interruptPolls(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ready := make(chan bool, 1)
	start := time.Now()
	go func() { ready <- waitReadable(int(r.Fd()), 100*time.Millisecond) }()
	select {
	case readable := <-ready:
		if readable || time.Since(start) < 75*time.Millisecond {
			t.Fatalf("interrupted empty pipe: readable=%v after %v; want the original timeout", readable, time.Since(start))
		}
	case <-time.After(500 * time.Millisecond):
		_, _ = w.WriteString("x") // release the poll before failing
		<-ready
		t.Fatal("signals extended a 100ms poll beyond 500ms")
	}
	if n := interrupts.Load(); n < 2 {
		t.Fatalf("poll was interrupted %d times; want several across the 100ms wait", n)
	}
}

func TestWaitReadableRetriesInterruptedPoll(t *testing.T) {
	if !canPoll {
		t.Skip("no select(2) wait on this platform")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	real, interrupted := pollOnce, make(chan struct{})
	var once sync.Once
	pollOnce = func(fd int, d time.Duration) (bool, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(interrupted)
			return false, syscall.EINTR
		}
		return real(fd, d)
	}
	t.Cleanup(func() { pollOnce = real })
	ready := make(chan bool, 1)
	go func() { ready <- waitReadable(int(r.Fd()), time.Second) }()
	<-interrupted
	time.Sleep(20 * time.Millisecond)
	if _, err := w.WriteString("[200~secret\x1b[201~\n"); err != nil {
		t.Fatal(err)
	}
	if !<-ready {
		t.Fatal("interrupted poll reported a timeout before the paste continuation arrived")
	}
}

// pselect6Number is SYS_PSELECT6 per linux arch; the syscall package names it only when building for linux.
var pselect6Number = map[string]string{"amd64": "270", "arm64": "72"}

// checkPollsWithPselect6 fails unless a thread parked in waitReadable is inside pselect6(2), read from /proc.
func checkPollsWithPselect6(t *testing.T, want string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	threads, done := make(chan string), make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		self, _ := os.Readlink("/proc/thread-self") // "<pid>/task/<tid>" of this locked thread
		threads <- self
		waitReadable(int(r.Fd()), 100*time.Millisecond)
	}()
	defer func() { <-done }()
	path := "/proc/" + <-threads + "/syscall"
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); runtime.Gosched() {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if fields := strings.Fields(string(data)); len(fields) > 0 && fields[0] == want {
			return
		}
	}
	t.Fatal("polling thread never entered select")
}

func TestLinuxARM64TestPackageCompiles(t *testing.T) {
	if want, ok := pselect6Number[runtime.GOARCH]; ok && runtime.GOOS == "linux" {
		checkPollsWithPselect6(t, want)
	}
	cmd := exec.Command("go", "test", "-c", "-o", filepath.Join(t.TempDir(), "tui.test"), ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("linux/arm64 test-package compilation: %v\n%s", err, output)
	}
}
