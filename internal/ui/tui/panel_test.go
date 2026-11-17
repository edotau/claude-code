package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/textutil"
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

// Left is wired to the same pickQuit signal as Esc/Back, so from any sub-page it lands on the first page.
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
	rows := layoutRows([][2]string{{"ab", "x"}, {"longname", "y"}}, 0)
	if got, want := rows[0], "ab              x"; got != want {
		t.Errorf("rows[0] = %q, want %q", got, want)
	}
	if got, want := rows[1], "longname        y"; got != want {
		t.Errorf("rows[1] = %q, want %q", got, want)
	}
}

// The panel draws where the cursor is and redraws by rewinding its own height; it must never home the cursor,

// render puts one blank line before the trailing Submit/Back row in every menu, not just PickBack ones.
func TestRenderBlankLineBeforeTrailingRow(t *testing.T) {
	s := pickState{rows: layoutRows([][2]string{{"a", "x"}, {"b", "y"}, {"Back", "return to the previous menu"}}, 0)}
	out := render("title", s, false)
	if !strings.Contains(out, "\x1b[K\r\n\x1b[K\r\n  "+s.rows[2]) {
		t.Errorf("render must blank-separate the trailing row from the rest:\n%q", out)
	}
	if got, want := panelLines("title", s, 200), len(s.rows)+5; got != want {
		t.Errorf("panelLines = %d, want %d (len(rows)+4, +1 for the trailing-row separator)", got, want)
	}
}

// A row too long for a narrow terminal wraps; panelLines must grow with it or redraws leave stale frames.
func TestPanelLinesCountsWrappedRows(t *testing.T) {
	s := pickState{rows: []string{"short", strings.Repeat("x", 90)}}
	wide := panelLines("title", s, 200)
	narrow := panelLines("title", s, 50) // the 90-char row wraps across 2 (2 prefix + 90 = 92/50); the hint still fits
	if wide != 7 {
		t.Errorf("panelLines(wide) = %d, want 7 (len(rows)+4 unwrapped, +1 trailing-row separator)", wide)
	}
	if narrow != wide+1 {
		t.Errorf("panelLines(narrow) = %d, want %d (the long row now spans 2 lines, not 1)", narrow, wide+1)
	}
}

// PickBack's index remap runs past Pick's TTY gate, so only Pick's non-TTY contract (-1 and a non-nil error) is tested.
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

// A sectioned panel has no title line; each section title follows a blank line and the cursor starts on a real row.
func TestSectionsOpenWithBlankLineBeforeEachTitle(t *testing.T) {
	s := pickState{rows: []string{"== agents ==", "a", "== tools ==", "b", "Enter"}, heads: map[int]bool{0: true, 2: true}}
	s = s.step(1)
	out := render("", s, false)
	if !strings.HasPrefix(out, "\x1b[K\r\n== agents ==") || !strings.Contains(out, "a\x1b[K\r\n\x1b[K\r\n== tools ==") {
		t.Errorf("section layout wrong:\n%q", out)
	}
	if s.sel != 1 || s.step(1).sel != 3 {
		t.Errorf("cursor must skip titles: sel=%d next=%d", s.sel, s.step(1).sel)
	}
	if got := strings.Count(out, "\r\n") + 1; got != panelLines("", s, 200) {
		t.Errorf("rendered %d lines, panelLines says %d", got, panelLines("", s, 200))
	}
}

func TestPanelLinesFullWidthSectionHeading(t *testing.T) {
	s := pickState{rows: []string{strings.Repeat("x", 80)}, heads: map[int]bool{0: true}}
	out := render("", s, false)
	if got, want := panelLines("", s, 80), strings.Count(out, "\r\n")+1; got != want {
		t.Fatalf("panelLines = %d, rendered rows = %d; heading has no marker prefix", got, want)
	}
}

// A section title wider than the terminal is clipped to one line, so panelLines must not count it as wrapped.
func TestPanelLinesClippedSectionHeading(t *testing.T) {
	s := pickState{rows: []string{"== a very long section title ==", "x", "Enter"}, heads: map[int]bool{0: true}, cols: 10}
	s = s.step(1)
	if got, want := panelLines("", s, 10), strings.Count(render("", s, false), "\r\n")+1; got != want {
		t.Fatalf("panelLines = %d, rendered lines = %d: a clipped heading over-counts and the rewind erases scrollback", got, want)
	}
}

// A panel taller than the screen shows only the rows that fit, so the rewind never reaches scrolled-off lines.
func TestFitWindowsRowsToTerminalHeight(t *testing.T) {
	rows := make([]string, 30)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	s := pickState{rows: rows, sel: 20}
	f := fit("", s, 120, 12)
	if n := panelLines("", f, 120); n >= 12 {
		t.Errorf("windowed panel is %d lines, want < 12", n)
	}
	if f.rows[f.sel] != "row 20" {
		t.Errorf("cursor row lost: %q", f.rows[f.sel])
	}
	if same := fit("", pickState{rows: rows[:3]}, 120, 12); len(same.rows) != 3 {
		t.Errorf("a panel that fits must not be windowed")
	}
}

// modelRows is a long model picker: a wide row, a CJK row and a trailing Back, laid out at cols-2 like pickHeads does.
func modelRows(cols int) []string {
	return layoutRows([][2]string{
		{"claude-sonnet-5.5", "Claude Sonnet 5.5  Balanced  vision · reasoning  " + strings.Repeat("x", 60)},
		{"claude-opus-5.5", "Claude Opus 5.5  Agentic coding  vision · reasoning"},
		{"gpt-6.1-sol", "GPT-6.1 Sol  日本語のタイトル日本語のタイトル  vision · reasoning"},
		{"Back", "return to the previous menu"},
	}, max(cols-2, 0))
}

func typed(t testing.TB, s pickState, text string) pickState {
	t.Helper()
	s, _ = handlePickKey(s, Key{Kind: KeyRune, Rune: '/'})
	for _, r := range text {
		s, _ = handlePickKey(s, Key{Kind: KeyRune, Rune: r})
	}
	return s
}

// frameText draws what pickHeads would for s and returns its lines with every escape sequence removed.
func frameText(title string, s pickState, cols, h int, color bool) []string {
	s.rows, s.hay, s.cols = modelRows(cols), lowerAll(modelRows(0)), cols
	out, _ := frame(title, s, cols, h, color)
	return strings.Split(csiRE.ReplaceAllString(out, ""), "\r\n")
}

// The breadcrumb, filter line, rows and footer each take one line of at most 80 cells, with colour on and off.
func TestFrameLinesFitEightyColumns(t *testing.T) {
	title := "configure › copilot › a very long model alias title that cannot fit on one eighty column line at all"
	for _, color := range []bool{false, true} {
		for _, text := range []string{"", "5.5", "zzz", strings.Repeat("日本", 40)} {
			s := pickState{}
			if text != "" {
				s = typed(t, pickState{rows: modelRows(80), hay: lowerAll(modelRows(0))}, text)
			}
			s.rows = modelRows(80)
			for _, line := range frameText(title, s, 80, 40, color) {
				if w := textutil.DisplayWidth(line); w > 80 {
					t.Errorf("color=%v filter=%q: %d cells > 80: %q", color, text, w, line)
				}
			}
		}
	}
	if got := frameText(title, pickState{}, 80, 40, false)[0]; !strings.HasSuffix(got, "…") {
		t.Errorf("a wider breadcrumb must clip with …, got %q", got)
	}
}

// "/" opens the filter, typing narrows the rows, Backspace widens them, Esc clears it; Enter picks the shown row.
func TestFilterNarrowsAndEscClears(t *testing.T) {
	s := typed(t, pickState{rows: modelRows(80), hay: lowerAll(modelRows(0))}, "opus")
	if v := s.view(); len(v.rows) != 1 || s.sel != 1 {
		t.Fatalf("filter opus: view %d rows, sel %d, want 1 row on row 1", len(v.rows), s.sel)
	}
	s, act := handlePickKey(s, Key{Kind: KeyEnter})
	if act != pickChoose || s.sel != 1 {
		t.Errorf("Enter on the filtered row = sel %d act %d, want the full-list index 1", s.sel, act)
	}
	if b := typed(t, pickState{rows: modelRows(80), hay: lowerAll(modelRows(0))}, "back"); b.sel != 3 {
		t.Errorf("filter back: sel %d, want the Back row's full-list index 3", b.sel)
	}
	s, _ = handlePickKey(s, Key{Kind: KeyEsc})
	if s.typing || s.query != "" || len(s.view().rows) != 4 {
		t.Errorf("Esc left typing=%v query=%q rows=%d, want a cleared filter over all 4 rows", s.typing, s.query, len(s.view().rows))
	}
	if _, act := handlePickKey(s, Key{Kind: KeyEsc}); act != pickQuit {
		t.Errorf("Esc with no filter = %d, want pickQuit as before", act)
	}
}

func TestFilterNoMatchesShowsEmptyRowAndBlocksEnter(t *testing.T) {
	s := typed(t, pickState{rows: modelRows(80), hay: lowerAll(modelRows(0))}, "zzz")
	lines := frameText("T", s, 80, 40, false)
	if !strings.Contains(strings.Join(lines, "\n"), "  no matches") {
		t.Errorf("empty result has no 'no matches' row:\n%s", strings.Join(lines, "\n"))
	}
	if _, act := handlePickKey(s, Key{Kind: KeyEnter}); act != pickNone {
		t.Errorf("Enter with nothing shown = %d, want pickNone", act)
	}
	if got, _ := frame("T", s, 80, 40, false); strings.Count(got, "\r\n")+1 != panelLines("T", s.view(), 80) {
		t.Errorf("panelLines disagrees with the rendered line count for an empty result")
	}
}

// A sectioned panel (no title) keeps a section title only above a surviving row, and counts its filter line.
func TestFilterSectionsKeepTitlesAboveMatchesAndCountLines(t *testing.T) {
	rows := []string{"== agents ==", "claude  ok", "codex  ok", "== tools ==", "tailscale  off", "Enter"}
	s := pickState{rows: rows, hay: rows, heads: map[int]bool{0: true, 3: true}, cols: 80}
	s = typed(t, s.step(1), "tail")
	out, lines := frame("", s, 80, 40, false)
	plain := csiRE.ReplaceAllString(out, "")
	if strings.Contains(plain, "agents") || !strings.Contains(plain, "== tools ==") || s.sel != 4 {
		t.Errorf("sel=%d; want only the tools title and its match:\n%s", s.sel, plain)
	}
	if got := strings.Count(out, "\r\n") + 1; got != lines {
		t.Errorf("rendered %d lines, panelLines says %d", got, lines)
	}
	if _, act := handlePickKey(s, Key{Kind: KeyEnter}); act != pickChoose || s.sel != 4 {
		t.Errorf("Enter on the filtered section row = sel %d act %d, want index 4", s.sel, act)
	}
}

// Without the filter, the frame keeps its rewind height and the old keys: q quits, j moves, "/" is the only new key.
func TestUnfilteredKeysAndLineCountUnchanged(t *testing.T) {
	s := pickState{rows: modelRows(80)}
	if _, act := handlePickKey(s, Key{Kind: KeyRune, Rune: 'q'}); act != pickQuit {
		t.Errorf("q = %d, want pickQuit", act)
	}
	if got, lines := frame("T", s, 80, 40, false); strings.Count(got, "\r\n")+1 != lines || lines != len(s.rows)+5 {
		t.Errorf("unfiltered frame = %d lines, panelLines %d, want len(rows)+5", strings.Count(got, "\r\n")+1, lines)
	}
	if h, _ := s.hint(); strings.Contains(h, "filter") {
		t.Errorf("a short menu's footer %q advertises the filter", h)
	}
	typing := typed(t, s, "")
	if _, act := handlePickKey(typing, Key{Kind: KeyRune, Rune: 'q'}); act != pickNone {
		t.Errorf("q while typing = %d, want it typed into the filter", act)
	}
}

// sttyOn runs stty on the terminal f, skipping where no stty exists.
func sttyOn(t *testing.T, f *os.File, args ...string) {
	t.Helper()
	stty := "/bin/stty" // hosttest's PATH allowlist leaves stty out; macOS ships it here, Linux here or in /usr/bin
	if _, err := os.Stat(stty); err != nil {
		stty = "/usr/bin/stty"
	}
	if _, err := os.Stat(stty); err != nil {
		t.Skipf("no stty: %v", err)
	}
	cmd := exec.Command(stty, args...)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stty %v: %v: %s", args, err, out)
	}
}

// testPickerTTY makes stdin an 80-column pty of the given height with input typed, and stdout a scratch file.
func testPickerTTY(t *testing.T, height int, input string) {
	t.Helper()
	master, slave := hosttest.PTY(t)
	sttyOn(t, slave, "rows", strconv.Itoa(height), "cols", "80")
	out, err := os.CreateTemp(t.TempDir(), "picker-output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, out
	t.Cleanup(func() { os.Stdin, os.Stdout = stdin, stdout })
	if _, err := master.WriteString(input); err != nil {
		t.Fatal(err)
	}
}

func TestPickEmptyOnTwoLineTerminal(t *testing.T) {
	testPickerTTY(t, 2, "q")
	if idx, err := Pick("", nil, 0); idx != -1 || err != nil {
		t.Fatalf("empty Pick on 80x2 = %d, %v; want -1, nil", idx, err)
	}
}

func TestPickClampsStartPastLastRow(t *testing.T) {
	testPickerTTY(t, 24, "\n")
	rows := [][2]string{{"a", ""}, {"b", ""}, {"c", ""}}
	if idx, err := Pick("", rows, 5); idx != 2 || err != nil {
		t.Fatalf("Pick(start=5) = %d, %v; want last row 2, nil", idx, err)
	}
}

func BenchmarkLayoutRows(b *testing.B) {
	rows := [][2]string{
		{"anthropic", "api.anthropic.com  oauth  [active]"},
		{"databricks", "adb-1234.azuredatabricks.net  m2m  [42m]"},
		{"bedrock", "us-east-1  iam  [expired]"},
		{"vertex", "us-central1  adc"},
	}
	for b.Loop() {
		_ = layoutRows(rows, 100)
	}
}

func BenchmarkPanelLines(b *testing.B) {
	s := pickState{
		rows:  []string{"HEAD", "alpha  detail [ok]", "beta  detail", "gamma  detail [warn]", "delta  detail"},
		heads: map[int]bool{0: true},
		sel:   1,
	}
	for b.Loop() {
		_ = panelLines("Providers", s, 80)
	}
}

func BenchmarkRender(b *testing.B) {
	s := pickState{rows: layoutRows([][2]string{
		{"anthropic", "api.anthropic.com  oauth  [active]"},
		{"databricks", "adb-1234.azuredatabricks.net  m2m  [42m]"},
		{"Back", "return to the previous menu"},
	}, 78), sel: 1}
	for b.Loop() {
		_ = render("Providers", s, true)
	}
}

func BenchmarkRenderCrumb(b *testing.B) {
	s := pickState{rows: layoutRows([][2]string{
		{"claude-sonnet-5.5", "Claude Sonnet 5.5  Balanced  vision · reasoning"},
		{"claude-opus-5.5", "Claude Opus 5.5  Agentic coding  vision · reasoning"},
		{"Back", "return to the previous menu"},
	}, 78), sel: 1}
	for b.Loop() {
		_ = render("configure › copilot › model", s, true)
	}
}

func BenchmarkFrameFiltered100(b *testing.B) {
	pairs := make([][2]string, 100)
	for i := range pairs {
		pairs[i] = [2]string{fmt.Sprintf("model-%d", i), "Claude Model  Balanced  vision · reasoning"}
	}
	rows := layoutRows(pairs, 78)
	s := typed(b, pickState{rows: rows, hay: lowerAll(layoutRows(pairs, 0)), cols: 80}, "model-4")
	for b.Loop() {
		_, _ = frame("configure › model", s, 80, 40, true)
	}
}
