package tui

import "testing"

// A fake key stream drives handlePickKey directly, so the panel's transitions test without a TTY.
func TestHandlePickKeyNavigatesAndClamps(t *testing.T) {
	s := pickState{rows: []string{"a", "b", "c"}}
	stream := []Key{{Kind: KeyUp}, {Kind: KeyDown}, {Kind: KeyDown}, {Kind: KeyDown}, {Kind: KeyDown}}
	for _, k := range stream {
		var act int
		s, act = handlePickKey(s, k)
		if act != pickNone {
			t.Fatalf("handlePickKey(%+v) acted %d mid-navigation, want pickNone", k, act)
		}
	}
	if s.sel != 2 {
		t.Errorf("sel = %d, want 2 (Down clamped at the last row)", s.sel)
	}
	s, act := handlePickKey(s, Key{Kind: KeyUp})
	if act != pickNone || s.sel != 1 {
		t.Errorf("after Up: sel=%d act=%d, want sel=1 act=pickNone", s.sel, act)
	}
}

func TestHandlePickKeyVimBindings(t *testing.T) {
	s := pickState{rows: []string{"a", "b"}}
	s, act := handlePickKey(s, Key{Kind: KeyRune, Rune: 'j'})
	if act != pickNone || s.sel != 1 {
		t.Errorf("'j' = sel %d act %d, want sel=1 act=pickNone", s.sel, act)
	}
	s, act = handlePickKey(s, Key{Kind: KeyRune, Rune: 'k'})
	if act != pickNone || s.sel != 0 {
		t.Errorf("'k' = sel %d act %d, want sel=0 act=pickNone", s.sel, act)
	}
}

func TestHandlePickKeyEnterChooses(t *testing.T) {
	s := pickState{rows: []string{"a", "b"}, sel: 1}
	s, act := handlePickKey(s, Key{Kind: KeyEnter})
	if act != pickChoose || s.sel != 1 {
		t.Errorf("Enter = sel %d act %d, want sel=1 act=pickChoose", s.sel, act)
	}
}

func TestHandlePickKeyQuit(t *testing.T) {
	for _, k := range []Key{{Kind: KeyEsc}, {Kind: KeyCtrlC}, {Kind: KeyRune, Rune: 'q'}} {
		s := pickState{rows: []string{"a", "b"}, sel: 1}
		_, act := handlePickKey(s, k)
		if act != pickQuit {
			t.Errorf("handlePickKey(%+v) = %d, want pickQuit", k, act)
		}
	}
}

func TestHandlePickKeyEnterOnEmptyRowsIsNoop(t *testing.T) {
	s := pickState{}
	_, act := handlePickKey(s, Key{Kind: KeyEnter})
	if act != pickNone {
		t.Errorf("Enter on empty rows = %d, want pickNone", act)
	}
}

func TestLayoutRowsAligns(t *testing.T) {
	rows := layoutRows([][2]string{{"ab", "x"}, {"longname", "y"}})
	if got, want := rows[0], "ab        x"; got != want {
		t.Errorf("rows[0] = %q, want %q", got, want)
	}
	if got, want := rows[1], "longname  y"; got != want {
		t.Errorf("rows[1] = %q, want %q", got, want)
	}
}
