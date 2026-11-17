package tree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
)

// fixtureTree is the testdata tree plus a long-named session and nested subagents, so column widths differ.
func fixtureTree(t *testing.T) (*Tree, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	tree, err := build(filepath.Join("testdata"), Options{Days: 99999, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	long := "a very long description that can never fit an eighty column terminal row, however it is clipped"
	tree.Sessions = append(tree.Sessions, Session{ID: "feedfacecafe", Cwd: "/Users/fake/demo-repo", Name: "named-session-with-a-long-label",
		Branch: "feature/a-long-branch-name", Title: "日本語のタイトル " + long, Updated: now.Add(-3 * time.Hour), Status: "idle",
		Subagents: []Subagent{
			{Type: "general-purpose", Status: "running", Depth: 1, Description: long, LastTool: "Bash", Updated: now.Add(-9 * time.Second),
				Tokens: Tokens{In: 22700, Out: 2900}, Dispatches: []Dispatch{{Agent: "codex", Model: "gpt-6", Status: "ok", Duration: "15m30.472s"}}},
			{Type: "opencode", Status: "stale", Depth: 2, Description: "é", Updated: now.Add(-5 * time.Hour), Tokens: Tokens{In: 5, Out: 1_200_000}},
		}})
	tree.Sessions[len(tree.Sessions)-1] = cacheSessionTotals(tree.Sessions[len(tree.Sessions)-1])
	return tree, now
}

// span is the display columns [start,end) one space-delimited field of a line covers.
type span struct{ start, end int }

func fields(line string) []span {
	var out []span
	in := false
	col := 0
	for _, r := range line {
		switch {
		case r != ' ' && !in:
			out, in = append(out, span{start: col}), true
		case r == ' ' && in:
			out[len(out)-1].end, in = col, false
		}
		col += textutil.DisplayWidth(string(r))
	}
	if in {
		out[len(out)-1].end = textutil.DisplayWidth(line)
	}
	return out
}

// Terminal rows stay within 80 columns, piped rows are whole; a level's fields line up on every row either way.
func TestRenderTextColumnsAlignAndFit(t *testing.T) {
	tree, now := fixtureTree(t)
	for _, width := range []int{80, 60, 0} {
		var buf strings.Builder
		renderTree(&buf, tree, now, width, "")
		var sessions, subagents, dispatches []string
		for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if n := textutil.DisplayWidth(line); width > 0 && n > width {
				t.Errorf("width %d: line %d is %d columns: %q", width, i, n, line)
			}
			if width == 0 && strings.Contains(line, "…") {
				t.Errorf("piped line %d is clipped: %q", i, line)
			}
			switch {
			case strings.Contains(line, "↑"):
				subagents = append(subagents, line)
			case strings.Contains(line, "↳"):
				dispatches = append(dispatches, line)
			case strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
				sessions = append(sessions, line)
			}
		}
		if len(sessions) < 2 || len(subagents) < 3 || len(dispatches) < 1 {
			t.Fatalf("width %d: got %d session, %d subagent, %d dispatch rows:\n%s", width, len(sessions), len(subagents), len(dispatches), buf.String())
		}
		assertAligned(t, width, "session", sessions, []int{1}, nil)
		assertAligned(t, width, "subagent", subagents, []int{1, 2}, []int{3, 4}) // type, state; ↑in, ↓out
		assertAligned(t, width, "dispatch", dispatches, []int{1, 2}, nil)
	}
}

// assertAligned checks fields starts[i] start and fields ends[i] end at the first row's column on every row.
func assertAligned(t *testing.T, width int, level string, rows []string, starts, ends []int) {
	t.Helper()
	first := fields(rows[0])
	for _, row := range rows[1:] {
		f := fields(row)
		for _, i := range starts {
			if f[i].start != first[i].start {
				t.Errorf("width %d: %s field %d starts at %d, want %d: %q", width, level, i, f[i].start, first[i].start, row)
			}
		}
		for _, i := range ends {
			if f[i].end != first[i].end {
				t.Errorf("width %d: %s field %d ends at %d, want %d: %q", width, level, i, f[i].end, first[i].end, row)
			}
		}
	}
}

func TestShortDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		-time.Second: "0s", 49*time.Second + 400*time.Millisecond: "49s", 7*time.Minute + 49*time.Second: "7m49s",
		15 * time.Minute: "15m", time.Hour + 45*time.Minute + 30*time.Second: "1h45m", 2 * time.Hour: "2h",
	} {
		if got := shortDuration(in); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", in, got, want)
		}
	}
	if got := durationText("15m30.472s"); got != "15m30s" {
		t.Errorf("durationText = %q, want 15m30s", got)
	}
}

type renderTestWriter func([]byte) (int, error)

func (w renderTestWriter) Write(p []byte) (int, error) { return w(p) }

func TestRenderWriteErrors(t *testing.T) {
	want := errors.New("output device full")
	renders := []struct {
		name string
		run  func(io.Writer) error
	}{
		{"text", func(w io.Writer) error {
			tr := &Tree{Sessions: []Session{{Cwd: "/repo"}}}
			return renderText(w, tr, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		}},
	}
	failures := []struct {
		name       string
		successful int
	}{{"first-write", 0}, {"partial-output", 1}}
	for _, render := range renders {
		for _, failure := range failures {
			t.Run(render.name+"/"+failure.name, func(t *testing.T) {
				calls := 0
				w := renderTestWriter(func(p []byte) (int, error) {
					calls++
					if calls <= failure.successful {
						return len(p), nil
					}
					return len(p) / 2, want
				})
				if err := render.run(w); !errors.Is(err, want) {
					t.Fatalf("render error = %v, want %v", err, want)
				}
				if calls != failure.successful+1 {
					t.Errorf("write calls = %d, want %d; writes must stop after failure", calls, failure.successful+1)
				}
			})
		}
	}
}

func TestDispatchCellsFlagsFallback(t *testing.T) {
	row := strings.Join(dispatchCells(Dispatch{Agent: "codex", Fallback: "claude"}, time.Now()), " ")
	if !strings.Contains(row, "claude→codex") || !strings.Contains(row, "⚠ fallback") {
		t.Errorf("dispatchCells(fallback) = %q, want the chain and the flag", row)
	}
}

// Root, child and grandchild nest by Parent with subtree sums in 80 columns; a legacy dispatch renders as before.
func TestRenderNestsDispatchesWithSubtreeTokens(t *testing.T) {
	use := func(in, out int) *wire.Usage { return &wire.Usage{InputTokens: in, OutputTokens: out} }
	legacy := Dispatch{ID: "legacy-codex", Agent: "codex", Model: "gpt-6", Status: "ok", Duration: "9s", Task: "old task"}
	chain := []Dispatch{
		{ID: "root-claude", Agent: "claude", Model: "sonnet", Status: "ok", Duration: "1m", Usage: use(1000, 500)},
		{ID: "child-codex", Agent: "codex", Model: "gpt-6", Status: "ok", Duration: "40s", Parent: "root-claude", Depth: 1, Usage: use(20000, 8000)},
		{ID: "grand-opencode", Agent: "opencode", Status: "ok", Duration: "9s", Parent: "child-codex", Depth: 2, Usage: use(40000, 6000)},
	}
	now := time.Now()
	render := func(width int, ds ...Dispatch) []string {
		var b strings.Builder
		renderTree(&b, &Tree{Dispatches: ds}, now, width, "")
		return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	}
	// legacy first, then the chain given grandchild-first: the children must still follow their parents
	lines := render(0, legacy, chain[2], chain[1], chain[0])
	for i, want := range []string{"legacy-codex", "root-claude", "└─ child-codex", "└─ grand-opencode"} {
		if i+1 >= len(lines) || !strings.Contains(lines[i+1], want) {
			t.Fatalf("row %d want %q in %q", i, want, strings.Join(lines, "\n"))
		}
	}
	for i, want := range []string{"1.5k Σ75.5k", "28k Σ74k", "46k"} {
		if got := lines[i+2]; !strings.Contains(got, want) || (i == 2 && strings.Contains(got, "Σ")) {
			t.Errorf("row %q: want tokens %q", got, want)
		}
	}
	if got, alone := strings.Fields(lines[1]), strings.Fields(render(0, legacy)[1]); !slices.Equal(got, alone) {
		t.Errorf("legacy row = %q, want %q as when alone", got, alone)
	}
	for _, line := range render(80, legacy, chain[0], chain[1], chain[2]) {
		if n := textutil.DisplayWidth(line); n > 80 {
			t.Errorf("row is %d columns, want <= 80: %q", n, line)
		}
	}
}

// A watch frame homes the cursor, clears each row's tail and the screen below, and never wipes the whole screen.
func TestWatchFramesRedrawInPlace(t *testing.T) {
	tree, now := fixtureTree(t)
	shrunk := &Tree{Sessions: tree.Sessions[:1]}
	var out strings.Builder
	writes := 0
	w := renderTestWriter(func(p []byte) (int, error) { writes++; return out.Write(p) })
	f := &frame{width: 80}
	var frames []string
	for _, tr := range []*Tree{tree, shrunk} {
		out.Reset()
		if err := f.draw(w, tr, now); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, out.String())
	}
	if writes != 2 {
		t.Errorf("%d writes for 2 frames, want one each", writes)
	}
	for i, fr := range frames {
		if strings.Contains(fr, "\x1b[2J") || !strings.HasPrefix(fr, "\x1b[H") || !strings.HasSuffix(fr, "\x1b[J") {
			t.Errorf("frame %d must home the cursor, end with clear-to-end-of-screen and not clear it all: %q", i, fr)
		}
		if strings.Count(fr, "\n") != strings.Count(fr, "\x1b[K\n") {
			t.Errorf("frame %d: every row must clear to its end before the newline", i)
		}
	}
	var plain strings.Builder
	renderTree(&plain, shrunk, now, 80, "")
	body := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(frames[1], "\x1b[H"), "\x1b[J"), "\x1b[K", "")
	if body != plain.String() {
		t.Errorf("shrunk frame body differs from the plain render:\n%q\n%q", body, plain.String())
	}
	if strings.Count(frames[1], "\n") >= strings.Count(frames[0], "\n") {
		t.Fatal("fixture did not shrink between frames")
	}
}

// watchFixture is a claudeDir with n running sessions of 4 subagents each, so a tick renders about 5n rows.
func watchFixture(tb testing.TB, n int) (string, time.Time) {
	tb.Helper()
	dir, now := tb.TempDir(), time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	src := filepath.Join("testdata", "projects", demoProj)
	head, err := os.ReadFile(filepath.Join(src, runningSID+".jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	sub := filepath.Join(src, finishedSID, "subagents", "agent-fakeagentid001")
	agentRaw, err := os.ReadFile(sub + ".jsonl")
	if err != nil {
		tb.Fatal(err)
	}
	metaRaw, err := os.ReadFile(sub + ".meta.json")
	if err != nil {
		tb.Fatal(err)
	}
	for i := range n {
		session := filepath.Join(dir, "projects", demoProj, fmt.Sprintf("session-%04d", i))
		if err := os.MkdirAll(filepath.Join(session, "subagents"), 0o700); err != nil {
			tb.Fatal(err)
		}
		files := map[string][]byte{session + ".jsonl": head}
		for j := range 4 {
			stem := filepath.Join(session, "subagents", fmt.Sprintf("agent-%04d", j))
			files[stem+".jsonl"], files[stem+".meta.json"] = agentRaw, metaRaw
		}
		for p, raw := range files {
			if err := os.WriteFile(p, raw, 0o600); err != nil {
				tb.Fatal(err)
			}
			if err := os.Chtimes(p, now, now); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return dir, now
}

// BenchmarkWatchTick is one redraw: "wipe" reloads the scan cache and clears the screen, "inplace" is the live loop.
func BenchmarkWatchTick(b *testing.B) {
	dir, now := watchFixture(b, 10)
	opts := Options{Days: 99999, Now: func() time.Time { return now }}
	for _, mode := range []string{"wipe", "inplace"} {
		b.Run(mode, func(b *testing.B) {
			o, f, bytesOut, writes := opts, &frame{width: 80}, 0, 0
			if mode == "inplace" {
				o.scans = loadScanCache()
			}
			w := renderTestWriter(func(p []byte) (int, error) { bytesOut, writes = bytesOut+len(p), writes+1; return len(p), nil })
			b.ReportAllocs()
			for b.Loop() {
				tr, err := listing(dir, o, true)
				if err != nil {
					b.Fatal(err)
				}
				if mode == "wipe" {
					fmt.Fprint(w, "\x1b[H\x1b[2J")
					renderTree(&textutil.ErrWriter{W: w}, tr, now, 80, "")
				} else {
					err = f.draw(w, tr, now)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(bytesOut)/float64(b.N), "bytes/frame")
			b.ReportMetric(float64(writes)/float64(b.N), "writes/frame")
		})
	}
}

// splitFixture lists an idle, a busy, a second idle and an idle session with a running dispatch, in that order.
func splitFixture(now time.Time) *Tree {
	sess := func(id, status string, subs ...Subagent) Session {
		return Session{ID: id, Cwd: "/repo", Branch: "main", Title: "task " + id, Updated: now, Status: status, Subagents: subs}
	}
	running := Subagent{Type: "codex", Status: "finished", Depth: 1, Updated: now, Dispatches: []Dispatch{{ID: "d1", Agent: "codex", Status: "running"}}}
	stale := Subagent{Type: "codex", Status: "stale", Depth: 1, Updated: now.Add(-time.Hour)}
	return &Tree{Sessions: []Session{
		sess("idle0001", "idle", stale), sess("busy0001", "active"), sess("idle0002", "idle"), sess("disp0001", "idle", running),
	}}
}

var escapes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// render returns tree's text rows at width, split on newlines.
func render(tree *Tree, now time.Time, width int, paint string) []string {
	var b strings.Builder
	renderTree(&b, tree, now, width, paint)
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// Active sessions (busy, or a running dispatch below) print first, one blank line, then the idle and stale ones.
func TestRenderSplitsActiveFromInactive(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	lines := render(splitFixture(now), now, 0, "")
	var order []string
	blanks := 0
	for _, line := range lines {
		switch {
		case line == "":
			blanks++
			order = append(order, "-")
		case strings.Contains(line, "busy0001"), strings.Contains(line, "disp0001"), strings.Contains(line, "idle0001"), strings.Contains(line, "idle0002"):
			order = append(order, strings.Fields(line)[0])
		}
	}
	if want := "busy0001 disp0001 - idle0001 idle0002"; strings.Join(order, " ") != want || blanks != 1 {
		t.Errorf("order = %q with %d blank lines, want %q with 1:\n%s", strings.Join(order, " "), blanks, want, strings.Join(lines, "\n"))
	}
}

// With nothing in one section there is no blank line, and no trailing one.
func TestRenderSingleSectionHasNoBlankLine(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	all := splitFixture(now)
	for name, sessions := range map[string][]Session{"active": all.Sessions[1:2], "inactive": all.Sessions[:1]} {
		if lines := render(&Tree{Sessions: sessions}, now, 0, ""); slices.Contains(lines, "") {
			t.Errorf("%s only: blank line in\n%s", name, strings.Join(lines, "\n"))
		}
	}
}

// Paint touches only the active section: the inactive rows are byte-identical and, unescaped, so are the active ones.
func TestRenderPaintsOnlyActiveRows(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	for _, width := range []int{0, 80, 30} {
		plain, painted := render(splitFixture(now), now, width, ""), render(splitFixture(now), now, width, "\x1b[35m")
		gap := slices.Index(plain, "")
		if gap < 0 || len(plain) != len(painted) {
			t.Fatalf("width %d: plain has %d rows, painted %d, blank at %d", width, len(plain), len(painted), gap)
		}
		if !slices.Equal(plain[gap:], painted[gap:]) {
			t.Errorf("width %d: inactive section changed:\n%q\n%q", width, plain[gap:], painted[gap:])
		}
		for i, line := range painted[:gap] {
			if got := escapes.ReplaceAllString(line, ""); got != plain[i] {
				t.Errorf("width %d: active row %d unescaped = %q, want %q", width, i, got, plain[i])
			}
		}
		if !strings.Contains(strings.Join(painted[:gap], ""), "\x1b[") {
			t.Errorf("width %d: no active row is painted", width)
		}
		for i, line := range painted {
			if n := textutil.DisplayWidth(escapes.ReplaceAllString(line, "")); width > 0 && n > width {
				t.Errorf("width %d: row %d is %d columns: %q", width, i, n, line)
			}
		}
	}
	if got := activePaint(&strings.Builder{}); got != "" {
		t.Errorf("activePaint on a non-terminal = %q, want none", got)
	}
}

// synthTree is n sessions of four subagents with one dispatch each, half of them busy: about 9n rows.
func synthTree(n int, now time.Time) *Tree {
	tree := &Tree{}
	for i := range n {
		s := Session{ID: fmt.Sprintf("sess%04d", i), Cwd: fmt.Sprintf("/repo/%d", i%7), Branch: "main", Title: "a task title", Updated: now, Status: "idle"}
		if i%2 == 0 {
			s.Status = "active"
		}
		for range 4 {
			status := "stale"
			if s.Status == "active" {
				status = "running"
			}
			s.Subagents = append(s.Subagents, Subagent{Type: "codex", Status: status, Depth: 1, Updated: now, LastTool: "Bash",
				Tokens: Tokens{In: 22700, Out: 2900}, Description: "a subagent description", Dispatches: []Dispatch{{ID: "d", Agent: "codex", Model: "gpt-6", Status: "ok", Duration: "9s"}}})
		}
		tree.Sessions = append(tree.Sessions, cacheSessionTotals(s))
	}
	return tree
}

// BenchmarkRender is the text render over about 900 rows: "plain" paints nothing, "paint" colours the active half.
func BenchmarkRender(b *testing.B) {
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	tree := synthTree(100, now)
	for name, paint := range map[string]string{"plain": "", "paint": "\x1b[38;2;156;192;159m"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				renderTree(io.Discard, tree, now, 80, paint)
			}
		})
	}
}

func TestParentTotalsCountUsageOnce(t *testing.T) {
	sub := Subagent{Tokens: Tokens{In: 100, Out: 20}, Dispatches: []Dispatch{
		{ID: "root", Usage: &wire.Usage{InputTokens: 30}},
		{ID: "child", Parent: "root", Usage: &wire.Usage{OutputTokens: 10}},
	}}
	s := cacheSessionTotals(Session{Tokens: sub.Tokens, Subagents: []Subagent{sub}})
	sub = s.Subagents[0]
	if got := sessionTotal(s); got != "Σ160" {
		t.Errorf("session total = %q", got)
	}
	if got := subTotal(sub); got != "Σ160" {
		t.Errorf("subagent total = %q", got)
	}
	var l layout
	run := cacheWorkflowTotal(WorkflowRun{Name: "run", Phases: []WorkflowPhase{{Name: "phase", Agents: []Subagent{sub}}}})
	addWorkflowRun(&l, run, time.Time{}, "", "")
	if got := strings.Join(l.lines(0, ""), "\n"); strings.Count(got, "Σ160") != 3 {
		t.Errorf("workflow, phase and subagent totals missing: %s", got)
	}

	sub.Description = "review the code: preserve this task prefix"
	if got := subTask(sub); got != sub.Description {
		t.Errorf("task prefix lost: %q", got)
	}
}

func TestRenderSessionTotalSurvivesActiveFilter(t *testing.T) {
	sub := Subagent{Status: statusRunning, Dispatches: []Dispatch{
		{ID: "running", Agent: "live", Status: statusRunning, Usage: &wire.Usage{InputTokens: 100}},
		{ID: "done1", Agent: "done1", Parent: "running", Status: "ok", Usage: &wire.Usage{InputTokens: 200}},
		{ID: "done2", Agent: "done2", Parent: "done1", Status: "ok", Usage: &wire.Usage{InputTokens: 300}},
	}}
	for _, workflow := range []bool{false, true} {
		s := Session{ID: "session", Status: statusActive, Subagents: []Subagent{sub}}
		wantTotals := 3
		if workflow {
			s.Subagents = nil
			s.Workflows = []WorkflowRun{{Status: statusRunning,
				Phases: []WorkflowPhase{{Name: "phase", Agents: []Subagent{sub}}}}}
			wantTotals = 5
		}
		s = cacheSessionTotals(s)
		for i, view := range []Session{activeSession(s), s} {
			var out strings.Builder
			renderTree(&out, &Tree{Sessions: []Session{view}}, time.Time{}, 80, "")
			rows := strings.Split(out.String(), "\n")
			if !strings.Contains(rows[1], "Σ600") {
				t.Errorf("workflow=%t detail=%t: session row = %q", workflow, i == 1, rows[1])
			}
			if got := strings.Count(out.String(), "Σ600"); got != wantTotals {
				t.Errorf("workflow=%t detail=%t: got %d totals: %s", workflow, i == 1, got, out.String())
			}
			if strings.Contains(out.String(), "done2") != (i == 1) {
				t.Errorf("detail=%t: wrong finished rows: %s", i == 1, out.String())
			}
			for _, row := range rows {
				if textutil.DisplayWidth(row) > 80 {
					t.Errorf("row exceeds 80 columns: %q", row)
				}
			}
		}
	}
}
