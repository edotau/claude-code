package statusline

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/ui/ansi"
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
	writeTokenExpiry(t, "ultra", time.Now().Add(42*time.Minute)) // a subscription session never shows it
	payload := fmt.Sprintf(`{"workspace":{"current_dir":%q},%s%s}`, filepath.Join(t.TempDir(), "proj"), twolineBody, extra)
	return strings.Split(plain(renderStatusline([]byte(payload), "twoline")), "\n")
}

// Both rows of the OAuth session: row 1 has the dir basename, provider, auth countdown and effort; row 2 context bar, tokens, 7d.
func TestTwolineRendersBothRows(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	rows := twolineRender(t, "dbx-u2m", fmt.Sprintf(`,"effort":{"level":"high"},`+
		`"rate_limits":{"seven_day":{"used_percentage":18,"resets_at":%d}}`, future))
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %q", rows)
	}
	if want := promptUser() + ":proj | databricks·dbx-u2m · u2m:42m · Fable 5.1:high"; rows[0] != want {
		t.Errorf("row 1 = %q, want %q", rows[0], want)
	}
	want := "ctx ▬▬▬▬▬▬▬▬▬─────────── 47% · 94.4k/200k · 7d ▬▬──────── 18%"
	if rows[1] != want {
		t.Errorf("row 2 = %q, want %q", rows[1], want)
	}
}

// No effort drops ":effort", no seven_day drops the 7d cell, and a subscription session (expiry recorded) has no auth cell or leading separator.
func TestTwolineCornerCases(t *testing.T) {
	rows := twolineRender(t, "ultra", "")
	if strings.HasSuffix(rows[0], ":") {
		t.Errorf("no effort, yet row 1 = %q", rows[0])
	}
	if strings.Contains(rows[1], "7d") || strings.Contains(rows[0], "u2m:") || strings.HasPrefix(rows[1], " ·") {
		t.Errorf("rows = %q, want no 7d, no auth, no leading separator", rows)
	}
	if !strings.HasPrefix(rows[1], "ctx ▬") {
		t.Errorf("row 2 should open with the context bar: %q", rows[1])
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

// Both rows are left-packed (no padding to a width), and row 1 carries no branch even when one was collected.
func TestTwolineNoBranchLeftPacked(t *testing.T) {
	v := values{userHost: "u", path: "proj", branch: "main", provider: "databricks·dbx-u2m", two: twoData{model: "M", auth: "u2m:9min", hasCtx: true, week: -1}}
	cfg := embeddedConfig()
	cfg.Width = 120
	lines := strings.Split(plain(renderTwoline(v, cfg, theme.Palettes{}, nil, "none")), "\n")
	if !strings.HasSuffix(lines[0], ":proj | databricks·dbx-u2m · u2m:9min · M") {
		t.Errorf("row 1 = %q, want head | provider · auth · model, no branch, no gap", lines[0])
	}
	if lines[1] != "ctx "+twoBar(0, 20)+" 0% · 0/0" {
		t.Errorf("row 2 = %q, want it left-packed", lines[1])
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

// A ramp-only palette (gemini) backs each cell and separator with a darker shade of its ramp color; padding stays bare.
func TestTwolineDerivedBackground(t *testing.T) {
	stops := []ansi.RGB{{200, 100, 50}}
	out := paintTwoRow([]twoSeg{{key: "model", text: "M"}, {key: "sep", text: twoSep}, {bare: true, text: "  "}}, nil, stops, "truecolor")
	bgs := regexp.MustCompile(`\x1b\[48;2;(\d+);(\d+);(\d+)m`).FindAllStringSubmatch(out, -1)
	if len(bgs) != 2 || strings.Count(out, "\x1b[48;") != 2 {
		t.Fatalf("want the model cell and separator on a background, padding bare: %q", out)
	}
	for _, bg := range bgs {
		r, g, b := atoi(t, bg[1]), atoi(t, bg[2]), atoi(t, bg[3])
		if r >= 200 || g >= 100 || b >= 50 || (r <= g || g <= b) {
			t.Errorf("bg %d;%d;%d is not a darker shade of the ramp's 200;100;50", r, g, b)
		}
	}
}

// atoi parses a decimal SGR field.
func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
