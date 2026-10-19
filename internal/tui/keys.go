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
)

// Key is one decoded keypress; Rune is set for KeyRune only.
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
	}
	if buf[0] < 0x20 {
		return dropped, 1 // unbound control byte (Tab, Backspace, ...): Pick has no use for it
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
	return dropped, 3
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
