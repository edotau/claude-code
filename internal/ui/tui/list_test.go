package tui

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
)

func ctrl(r rune) Key { return Key{Kind: KeyCtrl, Rune: r} }

func runes(s string) []Key {
	var ks []Key
	for _, r := range s {
		ks = append(ks, Key{Kind: KeyRune, Rune: r})
	}
	return ks
}

// feed drives listKey over keys, failing if any of them finishes the dialog.
func feed(t *testing.T, s listState, keys ...Key) listState {
	t.Helper()
	for _, k := range keys {
		var done bool
		var res ListResult
		if s, res, done = listKey(s, k); done {
			t.Fatalf("key %+v finished the list early: %+v", k, res)
		}
	}
	return s
}

func sampleSpec(search bool) ListSpec {
	return ListSpec{
		Title:  "opencode",
		Search: search,
		Rows: []ListRow{
			{Label: "GitHub Copilot", Detail: "OAuth", Status: "in use by claude-code", OK: true, Section: "accounts"},
			{Label: "+ Add account", Detail: "opencode auth login", Section: "actions"},
			{Label: "Nvidia", Detail: "API key", OK: true, Section: "accounts"},
			{Label: "GitHub Copilot", Detail: "$GITHUB_TOKEN", Status: "environment (unset to disconnect)", Section: "accounts"},
			{Label: "Use API key", Detail: "claude-code key for opencode (BYOK)", Section: "actions"},
		},
		Binds: []Bind{{Key: ctrl('d'), Name: "logout", Help: "log out", Confirm: "Press ctrl+d again to confirm"}},
	}
}

func TestDecodeControlAndBackspace(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want Key
		n    int
	}{
		{[]byte{0x04}, ctrl('d'), 1},
		{[]byte{0x12}, ctrl('r'), 1},
		{[]byte{0x7f}, Key{Kind: KeyBackspace}, 1},
		{[]byte{0x08}, Key{Kind: KeyBackspace}, 1},
		{[]byte("\x1b[3~"), dropped, 4}, // Delete: no stray '~' rune left behind
	} {
		if got, n := Decode(c.in); got != c.want || n != c.n {
			t.Errorf("Decode(%q) = %+v,%d want %+v,%d", c.in, got, n, c.want, c.n)
		}
	}
	if _, n := Decode([]byte("\x1b[3")); n != 0 {
		t.Errorf("incomplete parameterized CSI consumed %d, want 0", n)
	}
	if got := ctrl('d').Name(); got != "ctrl+d" {
		t.Errorf("Name = %q", got)
	}
}

func TestListGroupsBySectionFirstSeen(t *testing.T) {
	if got, want := newListState(sampleSpec(true), 0).order, []int{0, 2, 3, 1, 4}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestListFilterDropsEmptySectionHeader(t *testing.T) {
	s := feed(t, newListState(sampleSpec(true), 0), runes("nvid")...)
	if len(s.view) != 1 || s.view[0] != 2 {
		t.Fatalf("view = %v, want [2]", s.view)
	}
	out := renderList(listLines(s, false, 200, 50))
	if !strings.Contains(out, "== accounts ==") || strings.Contains(out, "== actions ==") {
		t.Errorf("the actions header must vanish with its rows:\n%s", out)
	}
	// Case-insensitive across Status and Section too; terms must all match.
	s = feed(t, newListState(sampleSpec(true), 0), runes("ENVIRON")...)
	if len(s.view) != 1 || s.view[0] != 3 {
		t.Errorf("status match: view = %v, want [3]", s.view)
	}
	s = feed(t, newListState(sampleSpec(true), 0), runes("actions byok")...)
	if len(s.view) != 1 || s.view[0] != 4 {
		t.Errorf("section+detail match: view = %v, want [4]", s.view)
	}
}

func TestListCursorClampsAndKeepsRowAfterFilter(t *testing.T) {
	spec := sampleSpec(true)
	s := newListState(spec, 4) // last row in grouped order
	if s.cur != 4 {
		t.Fatalf("start cursor = %d, want 4", s.cur)
	}
	s = feed(t, s, runes("github")...) // rows 0 and 3 survive; row 4 does not → clamp
	if s.cur != 1 || s.selected() != 3 {
		t.Errorf("clamped cursor = %d (row %d), want 1 (row 3)", s.cur, s.selected())
	}
	s = newListState(spec, 3) // row 3 survives the filter: the cursor stays on it
	s = feed(t, s, runes("git")...)
	if s.selected() != 3 {
		t.Errorf("kept row = %d, want 3", s.selected())
	}
	s = feed(t, s, Key{Kind: KeyBackspace}, Key{Kind: KeyBackspace}, Key{Kind: KeyBackspace})
	if len(s.view) != 5 || s.selected() != 3 || len(s.query) != 0 {
		t.Errorf("after backspacing: view=%v sel=%d query=%q", s.view, s.selected(), string(s.query))
	}
}

func TestListEnterReturnsOriginalIndex(t *testing.T) {
	s := feed(t, newListState(sampleSpec(true), 0), Key{Kind: KeyDown}, Key{Kind: KeyDown}, Key{Kind: KeyDown})
	_, res, done := listKey(s, Key{Kind: KeyEnter})
	if !done || res != (ListResult{Index: 1}) {
		t.Errorf("Enter = %+v,%v, want Index 1 (the 4th grouped row is original row 1)", res, done)
	}
}

func TestListJKOnlyWithoutSearch(t *testing.T) {
	s := feed(t, newListState(sampleSpec(false), 0), runes("jj")...)
	if s.cur != 2 {
		t.Errorf("j without search: cur = %d, want 2", s.cur)
	}
	s = feed(t, s, runes("k")...)
	if s.cur != 1 {
		t.Errorf("k without search: cur = %d, want 1", s.cur)
	}
	s = feed(t, newListState(sampleSpec(true), 0), runes("jk")...)
	if string(s.query) != "jk" || len(s.view) != 0 {
		t.Errorf("j/k with search must type: query=%q cur=%d", string(s.query), s.cur)
	}
	if _, res, done := listKey(newListState(sampleSpec(false), 0), Key{Kind: KeyRune, Rune: 'q'}); !done || res.Index != -1 {
		t.Errorf("q without search must go back")
	}
	if s, _, done := listKey(newListState(sampleSpec(true), 0), Key{Kind: KeyRune, Rune: 'q'}); done || string(s.query) != "q" {
		t.Errorf("q with search must type")
	}
}

func TestListBackKeys(t *testing.T) {
	for _, k := range []Key{{Kind: KeyEsc}, {Kind: KeyCtrlC}, {Kind: KeyLeft}} {
		for _, search := range []bool{true, false} {
			if _, res, done := listKey(newListState(sampleSpec(search), 2), k); !done || res != (ListResult{Index: -1}) {
				t.Errorf("%+v (search=%v) = %+v,%v, want back", k, search, res, done)
			}
		}
	}
}

func TestListDoublePressConfirm(t *testing.T) {
	d := ctrl('d')
	s := newListState(sampleSpec(true), 2)
	s, _, done := listKey(s, d)
	if done || s.pending != 0 || s.pendRow != 2 {
		t.Fatalf("first press: done=%v pending=%d row=%d", done, s.pending, s.pendRow)
	}
	if out := renderList(listLines(s, false, 200, 50)); !strings.Contains(out, "Press ctrl+d again to confirm") {
		t.Errorf("first press must show the prompt:\n%s", out)
	}
	if _, res, done := listKey(s, d); !done || res != (ListResult{Index: 2, Action: "logout"}) {
		t.Errorf("second press = %+v,%v, want row 2 logout", res, done)
	}
	// Any other key cancels; a later press starts over.
	s, _, _ = listKey(newListState(sampleSpec(true), 2), d)
	s = feed(t, s, ctrl('x')) // unbound and not a filter keystroke, so the row stays selected
	if s.pending != -1 {
		t.Errorf("another key must cancel the pending confirm")
	}
	if s, _, done = listKey(s, d); done || s.pending != 0 {
		t.Errorf("press after cancel must re-arm, not fire")
	}
	// A filter keystroke cancels too, even when the marked row survives the filter.
	s, _, _ = listKey(newListState(sampleSpec(true), 2), d)
	if s = feed(t, s, runes("n")...); s.pending != -1 || s.selected() != 2 {
		t.Errorf("typing must cancel: pending=%d sel=%d", s.pending, s.selected())
	}
	// Moving the cursor cancels, so the second press lands on a fresh row and only arms.
	s, _, _ = listKey(newListState(sampleSpec(true), 2), d)
	s = feed(t, s, Key{Kind: KeyDown})
	if s.pending != -1 {
		t.Errorf("cursor move must cancel the pending confirm")
	}
	if s, _, done = listKey(s, d); done || s.pendRow != 3 {
		t.Errorf("press on the new row must only arm it: done=%v row=%d", done, s.pendRow)
	}
}

func TestListBindWithoutConfirmFires(t *testing.T) {
	spec := sampleSpec(true)
	spec.Binds = append(spec.Binds, Bind{Key: ctrl('r'), Name: "rename", Help: "rename"})
	_, res, done := listKey(newListState(spec, 3), ctrl('r'))
	if !done || res != (ListResult{Index: 3, Action: "rename"}) {
		t.Errorf("ctrl+r = %+v,%v", res, done)
	}
}

func TestListEmptyResult(t *testing.T) {
	s := feed(t, newListState(sampleSpec(true), 0), runes("zzz")...)
	if len(s.view) != 0 || s.selected() != -1 {
		t.Fatalf("view = %v", s.view)
	}
	for _, k := range []Key{{Kind: KeyEnter}, ctrl('d'), {Kind: KeyDown}, {Kind: KeyUp}} {
		var done bool
		if s, _, done = listKey(s, k); done || s.pending != -1 {
			t.Errorf("%+v on an empty result must do nothing", k)
		}
	}
	out := renderList(listLines(s, false, 200, 50))
	if !strings.Contains(out, "  no matches") || strings.Contains(out, "==") {
		t.Errorf("empty result render:\n%s", out)
	}
	s = feed(t, s, Key{Kind: KeyBackspace}, Key{Kind: KeyBackspace}, Key{Kind: KeyBackspace})
	if s.selected() != 0 {
		t.Errorf("clearing the query must restore a cursor, got row %d", s.selected())
	}
}

func TestListRowMatchesTerms(t *testing.T) {
	row := ListRow{Label: "Alpha", Detail: "BETA", Status: "Ready", Section: "Grüppe"}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"", true},
		{" \t\n\u2003", true},
		{"ALPHA beta", true},
		{"ready\u2003GRÜPPE", true},
		{"beta beta", true},
		{"missing alpha beta", false},
		{"alpha missing", false},
		{"alphabeta", false},
	} {
		if got := row.matches(tc.query); got != tc.want {
			t.Errorf("matches(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestListFooter(t *testing.T) {
	spec := sampleSpec(true)
	if got, want := listFooter(spec), "↑/↓ select · enter confirm · type to search · esc back · ctrl+d log out"; got != want {
		t.Errorf("footer = %q\nwant %q", got, want)
	}
	spec.Search = false
	spec.Binds = append(spec.Binds, Bind{Key: ctrl('x'), Name: "hidden"}) // no Help → not listed
	if got, want := listFooter(spec), "↑/↓ select · enter confirm · esc back · ctrl+d log out"; got != want {
		t.Errorf("footer = %q\nwant %q", got, want)
	}
	spec.Footer = "custom"
	if listFooter(spec) != "custom" {
		t.Errorf("custom footer must be used verbatim")
	}
}

func TestListRenderSnapshot(t *testing.T) {
	s := feed(t, newListState(sampleSpec(true), 0), Key{Kind: KeyDown})
	got := strings.ReplaceAll(renderList(listLines(s, false, 200, 50)), "\x1b[K", "")
	want := strings.Join([]string{
		"opencode",
		"Search: ",
		"== accounts ==",
		"  GitHub Copilot  OAuth                                ✓ in use by claude-code",
		"▸ Nvidia          API key                              ✓",
		"  GitHub Copilot  $GITHUB_TOKEN                        environment (unset to disconnect)",
		"== actions ==",
		"  + Add account   opencode auth login",
		"  Use API key     claude-code key for opencode (BYOK)",
		"",
		"↑/↓ select · enter confirm · type to search · esc back · ctrl+d log out",
	}, "\r\n")
	if got != want {
		t.Errorf("render:\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("color off must emit no SGR")
	}
}

func TestListWindowsToTerminalHeight(t *testing.T) {
	spec := ListSpec{Title: "t", Search: true}
	for i := range 40 {
		spec.Rows = append(spec.Rows, ListRow{Label: "row" + string(rune('A'+i%26)), Section: []string{"a", "b"}[i/20]})
	}
	s := newListState(spec, 30)
	ls := listLines(s, false, 120, 12)
	if n := countLines(ls, 120); n >= 12 {
		t.Errorf("frame is %d lines, want < 12", n)
	}
	if !strings.Contains(renderList(ls), "▸ ") {
		t.Errorf("cursor row must stay in the window")
	}
}

func TestListNonTTY(t *testing.T) {
	if StdinIsTerminal() {
		t.Skip("stdin is a terminal")
	}
	if res, err := List(sampleSpec(true), 0); err == nil || res.Index != -1 {
		t.Errorf("List off a TTY = %+v,%v, want -1 and the Open error", res, err)
	}
}

func TestRunAttachedExitError(t *testing.T) {
	needSh(t)
	cancelled, err := RunAttached([]string{"sh", "-c", "exit 3"})
	var ee *exec.ExitError
	if cancelled || err == nil || !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("exit 3 = cancelled %v err %v, want false and exit status 3", cancelled, err)
	}
	if c, err := RunAttached([]string{"true"}); c || err != nil {
		t.Errorf("true = %v,%v", c, err)
	}
	if _, err := RunAttached(nil); err == nil {
		t.Errorf("empty argv must error")
	}
}

// The child interrupts this test process and then itself: the parent survives (signal swallowed) and the child's
// own SIGINT reads as cancelled.
func TestRunAttachedSignalCancels(t *testing.T) {
	needSh(t)
	cancelled, err := RunAttached([]string{"sh", "-c", "kill -INT $PPID; sleep 0.2; kill -INT $$; sleep 5"})
	if !cancelled || err != nil {
		t.Errorf("signalled child = cancelled %v err %v, want true,nil", cancelled, err)
	}
}

func needSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

func sectionedListState() listState {
	return newListState(ListSpec{Title: "T", Rows: []ListRow{{Label: "a", Section: "Alpha"}, {Label: "b", Section: "Beta"}}}, 0)
}

func sectionHead(ls []line) (string, bool) {
	for _, l := range ls {
		if strings.Contains(l.plain, "== Alpha ==") {
			return l.styled, true
		}
	}
	return "", false
}

// TestListSectionHeadUsesAccent: the "== Section ==" head is painted by Accent: fallback cyan, then palette truecolor.
func TestListSectionHeadUsesAccent(t *testing.T) {
	setPalette(t, theme.Palette{}, false)
	setTerm(t, "truecolor", "xterm-256color")
	got, ok := sectionHead(listLines(sectionedListState(), true, 80, 30))
	if !ok {
		t.Fatal("no section head line")
	}
	if want := ansi.Style(true, ansi.Bold+ansi.Cyan, "== Alpha =="); got != want {
		t.Errorf("fallback head = %q, want %q", got, want)
	}
	setPalette(t, theme.Palette{Accent: ansi.RGB{71, 150, 228}}, true)
	got, _ = sectionHead(listLines(sectionedListState(), true, 80, 30))
	if !strings.Contains(got, "38;2;71;150;228") {
		t.Errorf("themed head = %q, want truecolor accent 38;2;71;150;228", got)
	}
}

// TestListNewlineValueKeepsOneRowPerLine: a Detail with a newline folds to one terminal row, so countLines is exact.
func TestListNewlineValueKeepsOneRowPerLine(t *testing.T) {
	s := newListState(ListSpec{Title: "t", Rows: []ListRow{{Label: "k", Detail: "a\nb\tc"}}}, 30)
	ls := listLines(s, false, 120, 20)
	want := countLines(ls, 120)
	if got := strings.Count(renderList(ls), "\n") + 1; got > want {
		t.Errorf("rendered %d lines but countLines says %d", got, want)
	}
	if strings.Contains(renderList(ls), "a\nb") {
		t.Errorf("newline leaked into the frame")
	}
}
