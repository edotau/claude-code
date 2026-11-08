// Package tui is the minimal raw-terminal surface configure's interactive pickers need: raw mode,
// arrow-key decoding, and a redraw-in-place panel. Key decoding is pure, so it tests without a TTY.
package tui

import (
	"errors"
	"fmt"
	"os"
	"sync"
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

// Decode parses ONE key from buf, returning it and the bytes consumed (0 means "need more input": an
// incomplete escape sequence or a split UTF-8 rune at the buffer's end).
func Decode(buf []byte) (Key, int) {
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

// decodeEscape handles a lone Esc and the CSI arrow sequences (ESC [ A/B/C/D); anything else drops, since
// Pick has no other escape-driven key.
func decodeEscape(buf []byte) (Key, int) {
	if len(buf) == 1 {
		return Key{Kind: KeyEsc}, 1
	}
	if buf[1] != '[' {
		return dropped, 1
	}
	if len(buf) < 3 {
		return Key{}, 0 // "ESC [" with no final byte yet
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

// drain decodes every complete key in pending, returning them and the still-undecoded tail.
func drain(pending []byte) ([]Key, []byte) {
	var keys []Key
	for len(pending) > 0 {
		key, consumed := Decode(pending)
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

// Terminal is the raw-mode session: cursor hidden, keys decoded onto a channel. Close restores
// everything; callers defer it so a panic can't strand the shell in raw mode.
type Terminal struct {
	fd   int
	prev *termState
	out  *os.File
	in   *os.File
	keys chan Key
	stop chan struct{} // closed by Close — ends the key pump
	done chan struct{} // closed by the key pump when it exits
	once sync.Once
}

// Open puts stdin into raw mode and hides the cursor. Fails cleanly when stdin is not a TTY, so
// callers fall back to a line-mode prompt instead of crashing.
func Open() (*Terminal, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return nil, errors.New("stdin is not a terminal")
	}
	prev, err := makeRaw(fd)
	if err != nil {
		return nil, err
	}
	t := &Terminal{fd: fd, prev: prev, out: os.Stdout, in: os.Stdin, keys: make(chan Key, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	fmt.Fprint(t.out, "\x1b[?25l") // hide cursor
	go t.readKeys()
	return t, nil
}

// Close restores the terminal: cooked mode, cursor back, key pump stopped. Safe to call more than once.
func (t *Terminal) Close() {
	t.stopReader()
	fmt.Fprint(t.out, "\x1b[?25h")
	_ = restore(t.fd, t.prev)
}

// stopReader ends the key pump and waits for it: an orphaned read(2) on stdin would otherwise win the
// next line the user types after Close returns. Unpollable stdin keeps the old parked-forever reader.
func (t *Terminal) stopReader() {
	if t.stop == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
	if canPoll {
		<-t.done
	}
}

// Keys is the decoded keypress stream.
func (t *Terminal) Keys() <-chan Key { return t.keys }

// Size returns the terminal's current width and height (80x24 when the query fails).
func (t *Terminal) Size() (w, h int) {
	w, h, err := proc.Size(t.fd)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// readKeys pumps stdin through drain. A partial sequence at the buffer end carries into the next
// read, so an escape sequence split across reads still decodes. Exits when stopReader signals.
func (t *Terminal) readKeys() {
	defer close(t.done)
	var pending []byte
	buf := make([]byte, 64)
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		if !waitReadable(t.fd, pollTick) {
			continue // nothing typed yet — re-check stop instead of parking in read(2)
		}
		n, err := t.in.Read(buf)
		if n > 0 {
			var keys []Key
			keys, pending = drain(append(pending, buf[:n]...))
			for _, key := range keys {
				select {
				case t.keys <- key:
				case <-t.stop:
					return
				}
			}
		}
		if err != nil {
			close(t.keys)
			return
		}
	}
}
