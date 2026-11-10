package statusline

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// easeAt keeps the ends fixed, leaves exp 0/1 linear, and moves the middle toward the end colors when exp < 1.
func TestEaseAt(t *testing.T) {
	for _, exp := range []float64{0, 1, 0.5} {
		if easeAt(0, exp) != 0 || easeAt(1, exp) != 1 {
			t.Errorf("exp %v must keep the ends at 0 and 1", exp)
		}
	}
	if easeAt(0.5, 0) != 0.5 || easeAt(0.5, 1) != 0.5 {
		t.Error("exp 0 and 1 must be linear")
	}
	if got := easeAt(0.25, 0.5); got != 0.5 {
		t.Errorf("easeAt(0.25, 0.5) = %v, want 0.5", got)
	}
}

func gradientPayload(cwd string) string {
	return `{"cwd":"` + cwd + `","model":{"id":"claude-opus-5-5","display_name":"Opus 5.5"},` +
		`"context_window":{"used_percentage":31,"context_window_size":1000000,"total_input_tokens":309500},` +
		`"rate_limits":{"seven_day":{"used_percentage":15}}}`
}

// The default gradient line: twoline's ctx block, the display name, no 7d/total/branch, velocity only with changes.
func TestGradientLine(t *testing.T) {
	hermetic(t)
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=a@b", "-c", "user.name=x"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-qm", "f")
	const ctx = " | claude-opus-5-5 · ctx ━━━─────── 31.0% · 309.5k/1M 7d:15%"
	if got := plain(Render([]byte(gradientPayload(repo)), "gradient")); !strings.HasSuffix(got, ctx) {
		t.Errorf("clean tree: %q must end %q (no provider, id without claude-)", got, ctx)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("a\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Dir(stampFile("velocity", "x"))) // drop the clean tree's cached velocity
	if got := plain(Render([]byte(gradientPayload(repo)), "gradient")); !strings.HasSuffix(got, ctx+" | +2/-1") {
		t.Errorf("dirty tree: %q must end %q", got, ctx+" | +2/-1")
	}
}

// The gradient label is the raw payload id: claude- kept, a [1m] tag and a profile: prefix dropped, a routed label kept.
func TestRawModelID(t *testing.T) {
	for id, want := range map[string]string{
		"claude-opus-5-5": "claude-opus-5-5", "databricks-claude-opus-5-5": "databricks-claude-opus-5-5",
		"claude-opus-5-5[1m]": "claude-opus-5-5", "work:claude-opus-5-5": "claude-opus-5-5", "": "short",
	} {
		if got := rawModelID(Input{Model: struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}{ID: id}}, "short"); got != want {
			t.Errorf("rawModelID(%q) = %q, want %q", id, got, want)
		}
	}
	if got := rawModelID(Input{}, "opus ⇄acme"); got != "opus ⇄acme" {
		t.Errorf("routed label replaced: %q", got)
	}
}

// Empty bar cells take the separator dim, not the trough tone that all but vanished on the terminal background.
func TestGradientEmptyCellsVisible(t *testing.T) {
	hermetic(t)
	line := Render([]byte(gradientPayload(t.TempDir())), "gradient")
	sep := line[:strings.Index(line, "|")]
	dimSGR := sep[strings.LastIndex(sep, "\x1b["):]
	if strings.Count(line, dimSGR+"─") != 7 {
		t.Errorf("want 7 empty cells painted %q (the | color) at 31%%: %q", dimSGR, line)
	}
}

// Segments that individually fit wrap without clipping or losing text.
func TestGradientWrapsWholeSegments(t *testing.T) {
	hermetic(t)
	const dir = "/statusline-fixture"
	t.Setenv("COLUMNS", "300")
	wide := plain(Render([]byte(gradientPayload(dir)), "gradient"))
	for _, cols := range []int{40, 65, 80} {
		t.Setenv("COLUMNS", strconv.Itoa(cols))
		lines := strings.Split(plain(Render([]byte(gradientPayload(dir)), "gradient")), "\n")
		if len(lines) < 2 {
			t.Fatalf("%d cols: want a wrap, got %q", cols, lines)
		}
		assertGradientRows(t, wide, lines, cols-ccMargin)
	}
}

func assertGradientRows(t *testing.T, wide string, lines []string, width int) {
	t.Helper()
	rest := wide
	for _, line := range lines {
		if visibleWidth(line) > width || strings.HasPrefix(line, " ") {
			t.Fatalf("width %d: row overflows or starts with a space: %q", width, line)
		}
		at := strings.Index(rest, line)
		if at < 0 {
			t.Fatalf("row %q splits a segment; full line %q", line, wide)
		}
		if strings.TrimLeft(rest[:at], " ·|") != "" {
			t.Fatalf("row %q lost text from %q", line, rest)
		}
		rest = rest[at+len(line):]
	}
	if rest != "" {
		t.Errorf("lost %q", rest)
	}
}

// gradientEase is behavior, not a pinned value: for any ease, the cell a fraction f along the line wears the ramp's
// color at f^ease (0.45 puts the ¼ cell at 54% of the ramp, 1.0 at 25%, 1.5 at 13%).
func TestGradientEaseColorsAlongLine(t *testing.T) {
	hermetic(t)
	cfg := loadConfig()
	cfg.Order, cfg.Segments = []string{"model"}, map[string]bool{"path": false, "velocity": false, "model": true}
	pal := cfg.LiftPalette(theme.EmbeddedPalettes())
	stops := cfg.Stops(pal)
	v := values{model: strings.Repeat("m", 41)} // 41 cells: index 10, 20, 30 sit exactly ¼, ½, ¾ along
	sgr := regexp.MustCompile(`\x1b\[1;38;2;(\d+);(\d+);(\d+)m`)
	for _, ease := range []float64{0.45, 0.65, 1, 1.5} {
		cfg.GradientEase = ease
		cells := sgr.FindAllStringSubmatch(renderGradient(v, cfg, pal, stops, ansi.TrueColor, 0), -1)
		if len(cells) != 41 {
			t.Fatalf("ease %v: %d painted cells, want 41", ease, len(cells))
		}
		for _, i := range []int{10, 20, 30} {
			want := ansi.GradAt(stops, math.Pow(float64(i)/40, ease))
			if got := fmt.Sprintf("%s;%s;%s", cells[i][1], cells[i][2], cells[i][3]); got != fmt.Sprintf("%d;%d;%d", want[0], want[1], want[2]) {
				t.Errorf("ease %v, cell %d/40: rgb %s, want the ramp at %.0f%% (%v)", ease, i, got, 100*math.Pow(float64(i)/40, ease), want)
			}
		}
	}
}
