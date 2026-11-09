package tui

import (
	"os"
	"testing"
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
			got, n := Decode(c.buf)
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
		if _, n := Decode(buf); n != 0 {
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
		key, n := Decode(buf)
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
	if isTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a TTY in this environment; the no-TTY path isn't exercised here")
	}
	tm, err := Open()
	if err == nil {
		tm.Close()
		t.Fatal("Open() succeeded without a TTY on stdin")
	}
}

// TestDecodeRightArrow: ESC [ C is Right, not dropped.
func TestDecodeRightArrow(t *testing.T) {
	k, n := Decode([]byte{0x1b, '[', 'C'})
	if k.Kind != KeyRight || n != 3 || k.Name() != "right" {
		t.Fatalf("Decode(ESC [ C) = %+v,%d", k, n)
	}
}
