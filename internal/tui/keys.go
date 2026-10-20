// Package tui is the minimal raw-terminal surface configure's interactive pickers need: raw mode,
// arrow-key decoding, and a redraw-in-place panel. Key decoding is pure, so it tests without a TTY.
package tui

import "unicode/utf8"

// KeyKind names the keys Pick reacts to; anything else printable arrives as KeyRune with Rune set.
type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyEnter
	KeyEsc
	KeyUp
	KeyDown
	KeyLeft
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
	if buf[0] >= 0x01 && buf[0] <= 0x1a {
		return Key{Kind: KeyCtrl, Rune: rune('a' + buf[0] - 1)}, 1
	}
	if buf[0] < 0x20 {
		return dropped, 1 // NUL and 0x1c-0x1f: no key binds them
	}
	r, n := utf8.DecodeRune(buf)
	if r == utf8.RuneError && n == 1 && !utf8.FullRune(buf) {
		return Key{}, 0 // incomplete rune at the buffer's end — wait for the rest
	}
	return Key{Kind: KeyRune, Rune: r}, n
}

// decodeEscape handles a lone Esc and the CSI arrow sequences (ESC [ A/B); anything else drops, since
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
	switch buf[2] {
	case 'A':
		return Key{Kind: KeyUp}, 3
	case 'B':
		return Key{Kind: KeyDown}, 3
	case 'D':
		return Key{Kind: KeyLeft}, 3
	}
	if buf[2] >= '0' && buf[2] <= '9' { // parameterized CSI (Delete is ESC [ 3 ~): drop through its final byte
		for i := 3; i < len(buf); i++ {
			if buf[i] >= 0x40 && buf[i] <= 0x7e {
				return dropped, i + 1
			}
		}
		return Key{}, 0
	}
	return dropped, 3
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
