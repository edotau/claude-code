package statusline

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/ui/theme"

	"github.com/edotau/claude-code/internal/providers"
)

const twolineBody = `"model":{"display_name":"Fable 5.1"},"context_window":{"context_window_size":200000,` +
	`"total_input_tokens":94400,"used_percentage":47.2}`

func twolineRender(t *testing.T, session, extra string) []string {
	t.Helper()
	hermetic(t)
	t.Setenv(providers.EnvSessionProfile, session)
	useSessions(t,
		&providers.Provider{Name: "dbx-u2m", Provider: "databricks", Auth: providers.Auth{Type: providers.AuthOAuth}},
		&providers.Provider{Name: "dbx-key", Provider: "databricks", Auth: providers.Auth{Type: providers.AuthKey, Command: "x"}})
	writeTokenExpiry(t, "dbx-u2m", time.Now().Add(42*time.Minute+30*time.Second))
	writeTokenExpiry(t, "max", time.Now().Add(42*time.Minute)) // a subscription session never shows it
	payload := fmt.Sprintf(`{"workspace":{"current_dir":%q},%s%s}`, filepath.Join(t.TempDir(), "proj"), twolineBody, extra)
	return strings.Split(plain(Render([]byte(payload), "twoline")), "\n")
}

// Both rows of the OAuth session: row 1 has the dir basename, provider, auth countdown and effort; row 2 ctx, tokens, 7d.
func TestTwolineRendersBothRows(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	rows := twolineRender(t, "dbx-u2m", fmt.Sprintf(`,"effort":{"level":"high"},`+
		`"rate_limits":{"seven_day":{"used_percentage":18,"resets_at":%d}}`, future))
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %q", rows)
	}
	if want := promptUser() + ":proj | databricks·dbx-u2m · u2m:42min · Fable 5.1 / high"; rows[0] != want {
		t.Errorf("row 1 = %q, want %q", rows[0], want)
	}
	want := "ctx ━━━━━━━━━─────────── 47.2% · 94.4k/200k · 7d ━━──────── 18%"
	if rows[1] != want {
		t.Errorf("row 2 = %q, want %q", rows[1], want)
	}
}

// No effort drops " / ", no seven_day drops the 7d cell, and a subscription session (expiry recorded) has no auth cell or leading separator.
func TestTwolineCornerCases(t *testing.T) {
	rows := twolineRender(t, "max", "")
	if strings.Contains(rows[0], " / ") {
		t.Errorf("no effort, yet row 1 = %q", rows[0])
	}
	if strings.Contains(rows[1], "7d") || strings.Contains(rows[0], "u2m:") || strings.HasPrefix(rows[1], " ·") {
		t.Errorf("rows = %q, want no 7d, no auth, no leading separator", rows)
	}
	if !strings.HasPrefix(rows[1], "ctx ") {
		t.Errorf("row 2 should open with ctx: %q", rows[1])
	}
}

func TestCompactTokens(t *testing.T) {
	for n, want := range map[int]string{950: "950", 94400: "94.4k", 200000: "200k", 1200000: "1.2M", 2000000: "2M"} {
		if got := compactTokens(n); got != want {
			t.Errorf("compactTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// Velocity follows row 1's model after " | ": a zero side is omitted, and both zero drops the cell and its separator.
func TestTwolineVelocity(t *testing.T) {
	text := func(row []twoSeg) (s string) {
		var sSb74 strings.Builder
		for _, c := range row {
			sSb74.WriteString(c.text)
		}
		s += sSb74.String()
		return s
	}
	for in, want := range map[[2]int]string{{4, 2}: " | +4 -2", {4, 0}: " | +4", {0, 2}: " | -2", {0, 0}: ""} {
		if got := text(appendVelocity(nil, in[0], in[1])); got != want {
			t.Errorf("appendVelocity(%d, %d) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// With a width row 2 ends at the right edge and keeps its cells together; too narrow leaves it as is.
func TestTwolineAlignRight(t *testing.T) {
	row := []twoSeg{{text: "ctx"}, {key: "sep", text: twoSep}, {text: "7d"}}
	got := alignRight(row, 20)
	if segWidth(got) != 20 || got[0].text != strings.Repeat(" ", 12) || got[2].text != twoSep {
		t.Errorf("aligned row = %v, want 12 leading spaces then the cells unchanged", got)
	}
	if got := alignRight(row, 6); len(got) != len(row) {
		t.Errorf("too narrow: row padded: %v", got)
	}
}

// With a width, row 2 ends at the right edge; row 1 carries no branch even when one was collected.
func TestTwolineNoBranchRow2Right(t *testing.T) {
	v := values{branch: "main", provider: "databricks·dbx-u2m", two: twoData{model: "M", auth: "u2m:9min", hasCtx: true, week: -1}}
	out := plain(renderTwoline(v, embeddedConfig(), theme.Palettes{}, nil, "none", 60))
	lines := strings.Split(out, "\n")
	if strings.Contains(lines[0], "main") || !strings.Contains(lines[0], "databricks·dbx-u2m · u2m:9min · M") {
		t.Errorf("row 1 = %q, want provider · auth · model and no branch", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  ") || !strings.HasSuffix(lines[1], "0/0") || len([]rune(lines[1])) != 60 {
		t.Errorf("row 2 = %q, want it right-aligned to 60", lines[1])
	}
}

// A palette that colors a cell (minimal) paints its fg as well as its bg; an uncolored cell keeps the ramp.
func TestTwolinePaletteForeground(t *testing.T) {
	th := map[string]map[string]string{"auth": {"bg": "#2E2820", "fg": "#EED49F"}}
	out := paintTwoRow([]twoSeg{{key: "auth", text: "u2m:9min"}, {key: "x", text: "y"}}, th, nil, "truecolor")
	if !strings.Contains(out, "\x1b[38;2;238;212;159mu") {
		t.Errorf("auth cell lacks the palette fg #EED49F: %q", out)
	}
}
