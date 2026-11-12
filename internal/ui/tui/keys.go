// Package tui is the minimal raw-terminal surface configure's interactive pickers need: raw mode,
// arrow-key decoding, and a redraw-in-place panel. Key decoding is pure, so it tests without a TTY.
package tui

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/proc"
)

// KeyKind names the keys Pick reacts to; anything else printable arrives as KeyRune with Rune set.
type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyEnter
	KeyEsc
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyCtrlC
	KeyBackspace // DEL (0x7f) or Ctrl-H (0x08)
	KeyCtrl      // Ctrl+letter other than C/H/J/M; Rune is the lowercase letter ('d' for Ctrl-D)
)

// Key is one decoded keypress; Rune is set for KeyRune and KeyCtrl only.
type Key struct {
	Kind KeyKind
	Rune rune
}

// dropped is the zero Key an unbound control byte or unknown escape sequence decodes to.
var dropped = Key{Kind: KeyRune, Rune: 0}

// decode parses ONE key from buf, returning it and the bytes consumed (0 means "need more input": an
// incomplete escape sequence or a split UTF-8 rune at the buffer's end).
func decode(buf []byte) (Key, int) {
	if len(buf) == 0 {
		return Key{}, 0
	}
	switch buf[0] {
	case 0x03:
		return Key{Kind: KeyCtrlC}, 1
	case '\r', '\n':
		return Key{Kind: KeyEnter}, 1
	case 0x1b:
		return decodeEscape(buf)
	case 0x7f, 0x08:
		return Key{Kind: KeyBackspace}, 1
	}
	if buf[0] < 0x20 {
		return decodeCtrl(buf[0]), 1
	}
	return decodeRune(buf)
}

// decodeCtrl maps a control byte to Ctrl+letter; NUL and 0x1c-0x1f have no binding and drop.
func decodeCtrl(b byte) Key {
	if b >= 0x01 && b <= 0x1a {
		return Key{Kind: KeyCtrl, Rune: rune('a' + b - 1)}
	}
	return dropped
}

// decodeRune reads one printable rune; (Key{}, 0) when the buffer ends mid-rune.
func decodeRune(buf []byte) (Key, int) {
	r, n := utf8.DecodeRune(buf)
	if r == utf8.RuneError && n == 1 && !utf8.FullRune(buf) {
		return Key{}, 0 // incomplete rune at the buffer's end — wait for the rest
	}
	return Key{Kind: KeyRune, Rune: r}, n
}

// decodeEscape handles a lone Esc and CSI/SS3 arrow sequences (ESC [ or ESC O, A-D); anything else drops.
func decodeEscape(buf []byte) (Key, int) {
	if len(buf) == 1 {
		return Key{Kind: KeyEsc}, 1
	}
	if buf[1] != '[' && buf[1] != 'O' {
		return dropped, 1
	}
	if len(buf) < 3 {
		return Key{}, 0 // "ESC [" or "ESC O" with no final byte yet
	}
	if buf[1] == 'O' { // SS3 arrows (application cursor mode); other SS3 keys drop through their final byte
		if kind, ok := csiArrows[buf[2]]; ok {
			return Key{Kind: kind}, 3
		}
		return dropped, 3
	}
	if kind, ok := csiArrows[buf[2]]; ok {
		return Key{Kind: kind}, 3
	}
	if buf[2] >= '0' && buf[2] <= '9' { // parameterized CSI (Delete is ESC [ 3 ~): drop through its final byte
		return skipCSIParams(buf)
	}
	return dropped, 3
}

// csiArrows maps the final byte of ESC [ <byte> to the arrow key it names.
var csiArrows = map[byte]KeyKind{'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft}

// skipCSIParams drops a parameterized CSI sequence through its final byte; 0 when that byte hasn't arrived.
func skipCSIParams(buf []byte) (Key, int) {
	for i := 3; i < len(buf); i++ {
		if buf[i] >= 0x40 && buf[i] <= 0x7e {
			return dropped, i + 1
		}
	}
	return Key{}, 0
}

// Name is the key's footer spelling: "ctrl+d", "enter", "esc", or the rune itself.
func (k Key) Name() string {
	switch k.Kind {
	case KeyEnter:
		return "enter"
	case KeyEsc:
		return "esc"
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyLeft:
		return "left"
	case KeyRight:
		return "right"
	case KeyCtrlC:
		return "ctrl+c"
	case KeyBackspace:
		return "backspace"
	case KeyCtrl:
		return "ctrl+" + string(k.Rune)
	}
	return string(k.Rune)
}

// isLoneEsc reports whether buf is a bare Esc, which may open a sequence whose rest has not arrived yet.
func isLoneEsc(buf []byte) bool { return len(buf) == 1 && buf[0] == 0x1b }

// drain decodes every complete key in pending, returning them and the still-undecoded tail.
func drain(pending []byte) ([]Key, []byte) {
	var keys []Key
	for len(pending) > 0 {
		if isLoneEsc(pending) {
			break // held until the next read or a silent pollTick: the Esc key, or the start of a split sequence
		}
		key, consumed := decode(pending)
		if consumed == 0 {
			break // incomplete tail — wait for the next read
		}
		pending = pending[consumed:]
		if key.Kind == KeyRune && key.Rune == 0 {
			continue // dropped control byte / unknown sequence
		}
		keys = append(keys, key)
	}
	return keys, pending
}

// terminal is the raw-mode session: cursor hidden, keys decoded onto a channel. Close restores
// everything; callers defer it so a panic can't strand the shell in raw mode.
type terminal struct {
	fd      int
	prev    *termState
	out     *os.File
	in      *os.File
	keys    chan Key
	stop    chan struct{} // closed by Close — ends the key pump
	done    chan struct{} // closed by the key pump when it exits
	once    sync.Once
	unguard func() // stops the SIGTERM/SIGHUP restore guard
	// readErr is the read error that ended the key stream; written before keys closes.
	readErr error
}

// openTerminal puts stdin into raw mode and hides the cursor. Fails cleanly when stdin is not a TTY, so
// callers fall back to a line-mode prompt instead of crashing.
func openTerminal() (*terminal, error) {
	fd := int(os.Stdin.Fd())
	if !proc.IsTerminalFD(fd) {
		return nil, errors.New("stdin is not a terminal")
	}
	prev, err := makeRaw(fd)
	if err != nil {
		return nil, err
	}
	t := &terminal{fd: fd, prev: prev, out: os.Stdout, in: os.Stdin, keys: make(chan Key, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	fmt.Fprint(t.out, "\x1b[?25l") // hide cursor
	t.unguard = restoreOnSignal(func() { fmt.Fprint(t.out, "\x1b[?25h"); _ = restore(fd, prev) })
	go t.readKeys()
	return t, nil
}

// restoreOnSignal runs undo, then exits 128+signal, on SIGTERM/SIGHUP: they skip deferred Close and strand the tty raw.
func restoreOnSignal(undo func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	done, once := make(chan struct{}), sync.Once{}
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case s := <-ch:
			undo()
			os.Exit(128 + int(s.(syscall.Signal)))
		case <-done:
		}
	}()
	return func() { once.Do(func() { signal.Stop(ch); close(done) }) }
}

// Close restores the terminal: cooked mode, cursor back, key pump stopped. Safe to call more than once.
func (t *terminal) Close() error {
	t.stopReader()
	if t.unguard != nil {
		t.unguard()
	}
	fmt.Fprint(t.out, "\x1b[?25h")
	return restore(t.fd, t.prev)
}

// stopReader ends the key pump and waits for it: an orphaned read(2) on stdin would otherwise win the
// next line the user types after Close returns. Unpollable stdin keeps the old parked-forever reader.
func (t *terminal) stopReader() {
	if t.stop == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
	if canPoll {
		<-t.done
	}
}

// Keys is the decoded keypress stream.
func (t *terminal) Keys() <-chan Key { return t.keys }

// Err is the read error that ended the key stream; read it only after Keys is closed.
func (t *terminal) Err() error { return t.readErr }

// Size returns the terminal's current width and height (80x24 when the query fails).
func (t *terminal) Size() (w, h int) {
	w, h, err := proc.Size(t.fd)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// readKeys pumps stdin through drain. An incomplete sequence carries into the next read; a lone Esc is
// held for one pollTick of silence before it counts as the Esc key. Exits when stopReader signals.
func (t *terminal) readKeys() {
	defer close(t.done)
	var pending []byte
	buf := make([]byte, 64)
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		if isLoneEsc(pending) && t.escTimedOut() {
			pending = nil
			if !t.emit([]Key{{Kind: KeyEsc}}) {
				return
			}
		}
		if !waitReadable(t.fd, pollTick) {
			continue // nothing typed yet — re-check stop instead of parking in read(2)
		}
		n, err := t.in.Read(buf)
		if n > 0 {
			var keys []Key
			keys, pending = drain(append(pending, buf[:n]...))
			if !t.emit(keys) {
				return
			}
		}
		if err != nil {
			t.readErr = err
			close(t.keys)
			return
		}
	}
}

// escTimedOut reports whether a held lone Esc got no continuation within pollTick (always true unpollable).
func (t *terminal) escTimedOut() bool { return !canPoll || !waitReadable(t.fd, pollTick) }

// emit sends keys to the consumer; false when Close stopped the pump first.
func (t *terminal) emit(keys []Key) bool {
	for _, key := range keys {
		select {
		case t.keys <- key:
		case <-t.stop:
			return false
		}
	}
	return true
}
