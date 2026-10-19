package tui

import (
	"fmt"
	"strings"
	"testing"
)

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
	for _, k := range []Key{{Kind: KeyEsc}, {Kind: KeyCtrlC}, {Kind: KeyRune, Rune: 'q'}, {Kind: KeyLeft}} {
		s := pickState{rows: []string{"a", "b"}, sel: 1}
		_, act := handlePickKey(s, k)
		if act != pickQuit {
			t.Errorf("handlePickKey(%+v) = %d, want pickQuit", k, act)
		}
	}
}

// Left is wired to the same pickQuit signal as Esc/Back: a submenu's caller is the top menu, so from any
// sub-page Left lands back on the first page exactly the way picking Back already does.
func TestHandlePickKeyLeftIsBack(t *testing.T) {
	s := pickState{rows: []string{"a", "b", "Back"}, sel: 1}
	_, act := handlePickKey(s, Key{Kind: KeyLeft})
	if act != pickQuit {
		t.Errorf("Left = %d, want pickQuit (same as Back)", act)
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

// The panel draws where the cursor is and redraws by rewinding its own height; it must never home the cursor,
// which painted the menu over the top of the screen while the dialogue continued below.
func TestRenderDrawsInPlaceAndRewindsItsOwnHeight(t *testing.T) {
	s := pickState{rows: layoutRows([][2]string{{"a", "cred:ok x"}, {"b", "cred:missing y"}}), sel: 1}
	out := render("title", s, false)
	if strings.Contains(out, "\x1b[H") {
		t.Error("render must not home the cursor")
	}
	if strings.Count(out, "\r\n") != panelLines("title", s, 200)-1 {
		t.Errorf("render emits %d line breaks, want %d (panelLines-1)", strings.Count(out, "\r\n"), panelLines("title", s, 200)-1)
	}
	want := fmt.Sprintf("\r\x1b[%dA", panelLines("title", s, 200)-1)
	if rewind("title", s, 200) != want || erase("title", s, 200) != want+"\x1b[J" {
		t.Errorf("rewind/erase = %q / %q, want %q / %q", rewind("title", s, 200), erase("title", s, 200), want, want+"\x1b[J")
	}
	if strings.Contains(out, "\x1b[") && strings.Contains(out, "\x1b[32m") {
		t.Error("color off must paint nothing")
	}
	colored := render("title", s, true)
	if !strings.Contains(colored, "\x1b[32mcred:ok") || !strings.Contains(colored, "\x1b[7m") {
		t.Errorf("color on must paint status tokens and reverse the selected row:\n%q", colored)
	}
}

// A header line draws in place of the blank line under the title — panelLines (and so render's own
// line count) must not change whether a header is set or not, or PickTable's redraw would overdraw
// the wrong number of lines and leave residue from the previous frame.
func TestRenderHeaderLineCountUnchanged(t *testing.T) {
	plain := pickState{rows: layoutRows([][2]string{{"a", "x"}, {"b", "y"}})}
	withHeader := pickState{rows: plain.rows, header: "ID    Model"}
	outPlain := render("title", plain, false)
	outHeader := render("title", withHeader, false)
	if got, want := strings.Count(outHeader, "\r\n"), strings.Count(outPlain, "\r\n"); got != want {
		t.Errorf("header line count = %d, want %d (unchanged from no-header)", got, want)
	}
	if panelLines("title", withHeader, 200) != panelLines("title", plain, 200) {
		t.Errorf("panelLines(header) = %d, want %d", panelLines("title", withHeader, 200), panelLines("title", plain, 200))
	}
	if !strings.Contains(outHeader, "ID    Model") {
		t.Errorf("render with a header must draw it:\n%q", outHeader)
	}
}

// render puts one blank line between the last regular row and the trailing Submit/Back row every picker
// appends as its final row, in every menu — not just the ones built through PickBack.
func TestRenderBlankLineBeforeTrailingRow(t *testing.T) {
	s := pickState{rows: layoutRows([][2]string{{"a", "x"}, {"b", "y"}, {"Back", "return to the previous menu"}})}
	out := render("title", s, false)
	if !strings.Contains(out, "\x1b[K\r\n\x1b[K\r\n  "+s.rows[2]) {
		t.Errorf("render must blank-separate the trailing row from the rest:\n%q", out)
	}
	if got, want := panelLines("title", s, 200), len(s.rows)+5; got != want {
		t.Errorf("panelLines = %d, want %d (len(rows)+4, +1 for the trailing-row separator)", got, want)
	}
}

// A row too long for a narrow terminal wraps there; panelLines (and so rewind's up-count) must grow with
// it, or a redraw undercounts and leaves a stale copy of the previous frame behind in the scrollback —
// the "menu printed three times" symptom a real narrow terminal showed over a wide pty in CI.
func TestPanelLinesCountsWrappedRows(t *testing.T) {
	s := pickState{rows: []string{"short", strings.Repeat("x", 90)}}
	wide := panelLines("title", s, 200)  // every line fits on one terminal row
	narrow := panelLines("title", s, 40) // the 90-char row now wraps across 3 (2 prefix + 90 = 92/40)
	if wide != 7 {
		t.Errorf("panelLines(wide) = %d, want 7 (len(rows)+4 unwrapped, +1 trailing-row separator)", wide)
	}
	if narrow != wide+2 {
		t.Errorf("panelLines(narrow) = %d, want %d (the long row now spans 3 lines, not 1)", narrow, wide+2)
	}
}

// PickBack's own index remap ("Back" row, appended at len(rows), maps to -1; every other index passes
// through unchanged) only runs past Pick's TTY gate, so it can't be driven with a fake key stream the
// way handlePickKey is above (no injectable seam on Open/isTerminal here). What IS testable without a
// TTY is Pick's own documented non-TTY contract, which PickBack inherits unchanged: -1 and a non-nil
// error, regardless of how many rows (and therefore where the synthetic Back row would land).
func TestPickBackNonTTYReturnsMinusOneRegardlessOfRowCount(t *testing.T) {
	for _, rows := range [][][2]string{
		nil,
		{{"a", "x"}},
		{{"a", "x"}, {"b", "y"}, {"c", "z"}},
	} {
		idx, err := PickBack("title", rows, 0)
		if idx != -1 || err == nil {
			t.Errorf("PickBack(%d rows) on a non-terminal = (%d, %v), want (-1, non-nil)", len(rows), idx, err)
		}
	}
}
