package statusline

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
	payload := fmt.Sprintf(`{"workspace":{"current_dir":%q},%s%s}`, t.TempDir(), twolineBody, extra)
	return strings.Split(plain(Render([]byte(payload), "twoline")), "\n")
}

// Both rows of the OAuth session: provider label, effort, auth countdown, ctx bar, tokens and 7d are all present.
func TestTwolineRendersBothRows(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	rows := twolineRender(t, "dbx-u2m", fmt.Sprintf(`,"effort":{"level":"high"},`+
		`"rate_limits":{"seven_day":{"used_percentage":18,"resets_at":%d}}`, future))
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %q", rows)
	}
	if !strings.HasPrefix(rows[0], promptUser()+":") || !strings.Contains(rows[0], " | databricks·dbx-u2m ") {
		t.Errorf("row 1 lacks the provider label: %q", rows[0])
	}
	if !strings.HasSuffix(rows[0], " · Fable 5.1 / high") {
		t.Errorf("row 1 tail: %q", rows[0])
	}
	want := "auth:u2m 42min · ctx ━━━━━━━━━─────────── 47.2% · 94.4k/200k · 7d ━━──────── 18%"
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
	if strings.Contains(rows[1], "7d") || strings.Contains(rows[1], "auth:") || strings.HasPrefix(rows[1], " ·") {
		t.Errorf("row 2 = %q, want no 7d, no auth, no leading separator", rows[1])
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
		for _, c := range row {
			s += c.text
		}
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

// The twoline head collapses its path to the first and last elements; a two-element path is left whole.
func TestCollapsePath(t *testing.T) {
	for in, want := range map[string]string{"~/worktrees/.claude/feat": "~/.../feat", "~/.claude": "~/.claude", "/srv/a/b": "/.../b"} {
		if got := collapsePath(in); got != want {
			t.Errorf("collapsePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Row 1's head gives way first: a path that does not fit collapses, then trims, so the tail keeps its width.
func TestTwolineFitHead(t *testing.T) {
	cfg := embeddedConfig()
	v := values{userHost: "u", path: "~/worktrees/.claude/feature-branch"}
	if got := fitHead(v, cfg, 10, 80).path; got != v.path {
		t.Errorf("room to spare: path = %q, want it whole", got)
	}
	if got := fitHead(v, cfg, 10, 35).path; got != "~/.../feature-branch" { // room 35-10-3 = 22 = its head
		t.Errorf("collapse: path = %q", got)
	}
	got := fitHead(v, cfg, 10, 25)
	if w := headWidth(got, cfg); w > 25-10-3 || !strings.HasSuffix(got.path, "…") {
		t.Errorf("trim: path = %q (head %d wide), want ≤ 12 ending in …", got.path, w)
	}
}
