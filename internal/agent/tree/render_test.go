package tree

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	return tree, now
}

// span is the display columns [start,end) one space-delimited field of a line covers.
type span struct{ start, end int }

func fields(line string) []span {
	var out []span
	in := false
	for col, r := range []rune(line) {
		switch {
		case r != ' ' && !in:
			out, in = append(out, span{start: col}), true
		case r == ' ' && in:
			out[len(out)-1].end, in = col, false
		}
	}
	if in {
		out[len(out)-1].end = utf8.RuneCountInString(line)
	}
	return out
}

// Terminal rows stay within 80 columns, piped rows are whole; a level's fields line up on every row either way.
func TestRenderTextColumnsAlignAndFit(t *testing.T) {
	tree, now := fixtureTree(t)
	for _, width := range []int{80, 60, 0} {
		var buf strings.Builder
		renderTree(&buf, tree, now, width)
		var sessions, subagents, dispatches []string
		for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if n := utf8.RuneCountInString(line); width > 0 && n > width {
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
