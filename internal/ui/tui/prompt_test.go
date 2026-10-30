package tui

import "testing"

// TestConfirmKey: y answers yes; n, q, Esc and Ctrl-C cancel at once; Enter takes the default; other keys wait.
func TestConfirmKey(t *testing.T) {
	cases := []struct {
		k         Key
		def, want bool
		done      bool
	}{
		{Key{Kind: KeyRune, Rune: 'y'}, false, true, true},
		{Key{Kind: KeyRune, Rune: 'n'}, true, false, true},
		{Key{Kind: KeyEsc}, true, false, true},
		{Key{Kind: KeyCtrlC}, true, false, true},
		{Key{Kind: KeyEnter}, false, false, true},
		{Key{Kind: KeyEnter}, true, true, true},
		{Key{Kind: KeyRune, Rune: 'x'}, false, false, false},
	}
	for _, c := range cases {
		if got, done := confirmKey(c.k, c.def); got != c.want || done != c.done {
			t.Errorf("confirmKey(%+v, def=%v) = %v,%v want %v,%v", c.k, c.def, got, done, c.want, c.done)
		}
	}
}
