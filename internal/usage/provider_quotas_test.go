package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// TestMain keeps every Run in this package offline: the live quota fetch is stubbed out.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	fetchQuotas = func() []quota { return nil }
	recordQuotaState = func([]quota) {}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestParseCopilot(t *testing.T) {
	body := `{"copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":
		{"entitlement":30000,"credits_used":41,"percent_remaining":99.9,"unlimited":false}}}`
	got := parseCopilot("bi-user", []byte(body))
	want := quota{Provider: "github", Account: "bi-user", Plan: "Business", Window: "monthly",
		UsedPct: 0.1, Used: 41, Limit: 30000, Resets: "2026-11-01"}
	if got != want {
		t.Errorf("parseCopilot = %+v, want %+v", got, want)
	}
	if u := parseCopilot("x", []byte(`{"quota_snapshots":{"premium_interactions":{"unlimited":true}}}`)); u.Note != "unlimited" {
		t.Errorf("unlimited seat note = %q", u.Note)
	}
	if bad := parseCopilot("x", []byte(`not json`)); !strings.HasPrefix(bad.Note, "unavailable") {
		t.Errorf("bad body note = %q", bad.Note)
	}
}

func TestParseWham(t *testing.T) {
	body := `{"email":"a@b.c","plan_type":"plus","rate_limit":{
		"primary_window":{"used_percent":13,"limit_window_seconds":18000,"reset_at":1792462523},
		"secondary_window":{"used_percent":23,"limit_window_seconds":604800,"reset_at":1792942448}}}`
	got := parseWham([]byte(body))
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(got), got)
	}
	if got[0].Window != "5h" || got[0].UsedPct != 13 || got[0].Plan != "Plus" || got[0].Account != "a@b.c" {
		t.Errorf("primary = %+v", got[0])
	}
	if got[1].Window != "weekly" || got[1].UsedPct != 23 {
		t.Errorf("secondary = %+v", got[1])
	}
	if none := parseWham([]byte(`{"plan_type":"plus","rate_limit":{}}`)); len(none) != 1 || none[0].Note == "" {
		t.Errorf("no-window body = %+v", none)
	}
}

// TestUsageDropsZeroRowsAndPrintsQuotas: a zero-token model ("<synthetic>") is dropped; quota rows follow the table.
func TestUsageDropsZeroRowsAndPrintsQuotas(t *testing.T) {
	isolateAgentStores(t)
	stub := []quota{
		{Provider: "github", Account: "me", Plan: "Pro+", Window: "monthly", UsedPct: 11, Used: 750, Limit: 7000, Resets: "2026-11-01"},
		{Provider: "google", Plan: "Antigravity", Note: "agy not logged in (run agy login)"},
	}
	orig := fetchQuotas
	fetchQuotas = func() []quota { return stub }
	t.Cleanup(func() { fetchQuotas = orig })

	claudeDir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	writeUsageFixture(t, claudeDir, "proj", "sess", time.Minute,
		usageRecordLine("claude-opus-5", 10, 5, ts), usageRecordLine("<synthetic>", 0, 0, ts))

	var out, errw bytes.Buffer
	if code := Run([]string{"--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	s := out.String()
	if strings.Contains(s, "<synthetic>") {
		t.Errorf("zero-token row printed:\n%s", s)
	}
	// default --show is "used": USAGE column + "N / M credits" detail.
	for _, want := range []string{"claude-opus-5", "PROVIDER QUOTAS", "USAGE", "750 / 7000 credits", "agy not logged in (run agy login)", "API KEYS"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}

	out.Reset()
	if code := Run([]string{"--days", "1", "--format", "json"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("json exit %d", code)
	}
	var doc struct {
		Models    map[string]json.RawMessage `json:"models"`
		Providers []quota                    `json:"providers"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Models["<synthetic>"]; ok || len(doc.Providers) != 2 {
		t.Errorf("json models=%v providers=%d", doc.Models, len(doc.Providers))
	}

	out.Reset()
	if code := Run([]string{"--days", "1", "--providers=false"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("offline exit %d", code)
	}
	if strings.Contains(out.String(), "PROVIDER QUOTAS") {
		t.Error("--providers=false still printed quotas")
	}
}

// TestSetStatesBoundaries: the default threshold (80) draws ok|warn|exceeded exactly at the documented
// boundaries — 79.9 stays ok, 80.0 (the threshold itself) is already warn, 100.0 and anything past it
// is exceeded, never "over-exceeded".
func TestSetStatesBoundaries(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{79.9, "ok"},
		{80.0, "warn"},
		{99.9, "warn"},
		{100.0, "exceeded"},
		{150.0, "exceeded"},
	}
	for _, c := range cases {
		qs := []quota{{Provider: "github", UsedPct: c.pct}}
		setStates(qs, 80)
		if qs[0].State != c.want {
			t.Errorf("setStates(pct=%.1f, threshold=80) = %q, want %q", c.pct, qs[0].State, c.want)
		}
	}
}

// TestSetStatesSkipsNoteRows: a row with a Note (lookup failed, unlimited, …) carries no numeric meaning —
// setStates must leave its State empty rather than misreading a zero-value UsedPct as "ok".
func TestSetStatesSkipsNoteRows(t *testing.T) {
	qs := []quota{{Provider: "google", Note: googleNote}}
	setStates(qs, 80)
	if qs[0].State != "" {
		t.Errorf("Note row State = %q, want empty", qs[0].State)
	}
}

// TestWarnThresholdEnvOverride: HARNESS_USAGE_WARN_PCT moves the warn boundary, both directions.
func TestWarnThresholdEnvOverride(t *testing.T) {
	t.Setenv("HARNESS_USAGE_WARN_PCT", "50")
	if got := warnThreshold(); got != 50 {
		t.Fatalf("warnThreshold() = %v, want 50", got)
	}
	qs := []quota{{Provider: "github", UsedPct: 60}}
	setStates(qs, warnThreshold())
	if qs[0].State != "warn" {
		t.Errorf("State at 60%% with threshold 50 = %q, want warn", qs[0].State)
	}
}

// TestWarnThresholdInvalidFallsBackToDefault: a non-numeric or out-of-range env value is treated as unset.
func TestWarnThresholdInvalidFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"not-a-number", "-5", "0", "150"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("HARNESS_USAGE_WARN_PCT", v)
			if got := warnThreshold(); got != 80 {
				t.Errorf("warnThreshold() with env %q = %v, want fallback 80", v, got)
			}
		})
	}
}

// TestBarRendering: 10-cell bar, floor-divided from the rounded percent, with a minimum of one filled
// cell for any positive percentage and a hard cap at 10 filled cells past 100%.
func TestBarRendering(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, "░░░░░░░░░░"},
		{1, "█░░░░░░░░░"}, // minimum one filled cell when >0, even below one cell's worth
		{50, "█████░░░░░"},
		{100, "██████████"},
		{150, "██████████"}, // capped, never more than 10 cells
	}
	for _, c := range cases {
		if got := bar(c.pct); got != c.want {
			t.Errorf("bar(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

// TestPrintQuotaWarnings: only warn/exceeded rows print to errw, in the documented format; ok rows are silent.
func TestPrintQuotaWarnings(t *testing.T) {
	qs := []quota{
		{Provider: "github", Account: "me", UsedPct: 50, State: "ok"},
		{Provider: "github", Account: "work", UsedPct: 85, State: "warn"},
		{Provider: "openai", Account: "", UsedPct: 100, State: "exceeded"},
	}
	var errw bytes.Buffer
	printQuotaWarnings(&errw, qs)
	got := errw.String()
	if strings.Contains(got, "me at 50%") {
		t.Errorf("an ok row must not warn:\n%s", got)
	}
	for _, want := range []string{
		"usage: github work at 85% (warn)\n",
		"usage: openai - at 100% (exceeded)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestPrintQuotasStateColumn: the STATE column renders the row's state, or "-" for a Note row.
func TestPrintQuotasStateColumn(t *testing.T) {
	qs := []quota{
		{Provider: "github", Account: "me", UsedPct: 85, State: "warn"},
		{Provider: "google", Note: googleNote},
	}
	var out bytes.Buffer
	printSubscriptionQuotas(&out, qs, "used")
	s := out.String()
	if !strings.Contains(s, "STATE") {
		t.Errorf("missing STATE header:\n%s", s)
	}
	if !strings.Contains(s, "warn") {
		t.Errorf("missing warn state value:\n%s", s)
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "   -  ") && !strings.Contains(last, " -  ") {
		t.Errorf("Note row STATE column should render \"-\", got line:\n%q", last)
	}
}

// TestUsageRunWarnsOnExceededState: an end-to-end Run() with a quota stub past threshold writes the STATE
// column to stdout, the warn line to stderr, and "state" in --format json — the full C wiring, not just
// the helpers in isolation.
func TestUsageRunWarnsOnExceededState(t *testing.T) {
	isolateAgentStores(t)
	stub := []quota{{Provider: "github", Account: "me", Plan: "Pro", Window: "monthly", UsedPct: 100, Used: 7000, Limit: 7000}}
	orig := fetchQuotas
	fetchQuotas = func() []quota { return stub }
	t.Cleanup(func() { fetchQuotas = orig })

	var out, errw bytes.Buffer
	if code := Run(nil, filepath.Join(t.TempDir(), "empty"), &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "exceeded") {
		t.Errorf("stdout missing exceeded state:\n%s", out.String())
	}
	if !strings.Contains(errw.String(), "usage: github me at 100% (exceeded)") {
		t.Errorf("stderr missing warn line:\n%s", errw.String())
	}

	out.Reset()
	errw.Reset()
	if code := Run([]string{"--format", "json"}, filepath.Join(t.TempDir(), "empty"), &out, &errw); code != 0 {
		t.Fatalf("json exit %d", code)
	}
	var doc struct {
		Providers []quota `json:"providers"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Providers) != 1 || doc.Providers[0].State != "exceeded" {
		t.Errorf("json providers = %+v, want one exceeded row", doc.Providers)
	}
}

// TestUsageQuotasWithoutTranscripts: no transcripts still prints the provider block.
func TestUsageQuotasWithoutTranscripts(t *testing.T) {
	orig := fetchQuotas
	fetchQuotas = func() []quota {
		return []quota{{Provider: "google", Plan: "Antigravity", Note: "agy not logged in (run agy login)"}}
	}
	t.Cleanup(func() { fetchQuotas = orig })
	var out, errw bytes.Buffer
	if code := Run(nil, filepath.Join(t.TempDir(), "empty"), &out, &errw); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "PROVIDER QUOTAS") {
		t.Errorf("no quota block:\n%s", out.String())
	}
}

func quotaRows(t *testing.T, show string, qs []quota) []string {
	t.Helper()
	var buf bytes.Buffer
	printSubscriptionQuotas(&buf, qs, show)
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

func TestQuotaDefaultShowIsUsageHeader(t *testing.T) {
	claudeDir := t.TempDir()
	orig := fetchQuotas
	fetchQuotas = func() []quota {
		return []quota{{Provider: "github", Window: "monthly", UsedPct: 11, Used: 750, Limit: 7000}}
	}
	t.Cleanup(func() { fetchQuotas = orig })
	var out, errw bytes.Buffer
	if code := Run(nil, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	s := out.String()
	if !strings.Contains(s, " USAGE ") || strings.Contains(s, " LEFT ") {
		t.Errorf("default header must be USAGE, not LEFT:\n%s", s)
	}
}

func TestQuotaBarPrecedesPercent(t *testing.T) {
	rows := quotaRows(t, "used", []quota{{Provider: "github", Window: "monthly", UsedPct: 50, Limit: 10, Used: 5}})
	row := rows[len(rows)-1]
	b, p := strings.Index(row, "█"), strings.Index(row, "50%")
	if b < 0 || p < 0 || b > p {
		t.Errorf("bar must precede percent (bar@%d pct@%d): %q", b, p, row)
	}
}

func TestQuotaPercentsRightAligned(t *testing.T) {
	for _, show := range []string{"used", "left"} {
		qs := []quota{
			{Provider: "a", UsedPct: 2, LeftPct: 2, Limit: 1},
			{Provider: "b", UsedPct: 16, LeftPct: 16, Limit: 1},
			{Provider: "c", UsedPct: 100, LeftPct: 100, Limit: 1},
		}
		rows := quotaRows(t, show, qs)[3:]
		ends := map[int]bool{}
		for i, pct := range []string{"2%", "16%", "100%"} {
			if !strings.Contains(rows[i], pct) {
				t.Fatalf("%s: row %d lacks %s: %q", show, i, pct, rows[i])
			}
			ends[len([]rune(rows[i][:strings.Index(rows[i], pct)+len(pct)]))] = true
		}
		if len(ends) != 1 {
			t.Errorf("%s: percents end at different columns: %q", show, rows)
		}
	}
}

func TestQuotaShowLeftHeaderAndComplement(t *testing.T) {
	q := quota{Provider: "github", Window: "monthly", UsedPct: 25, LeftPct: 75, Used: 1750, Limit: 7000}
	rows := quotaRows(t, "left", []quota{q})
	if !strings.Contains(rows[2], "LEFT") || strings.Contains(rows[2], "USAGE") {
		t.Errorf("left header: %q", rows[2])
	}
	if r := rows[3]; !strings.Contains(r, "75%") || !strings.Contains(r, "5250 of 7000 credits left") {
		t.Errorf("left row: %q", r)
	}
	u := quotaRows(t, "used", []quota{q})
	if r := u[3]; !strings.Contains(r, "25%") || !strings.Contains(r, "1750 / 7000 credits") {
		t.Errorf("used row: %q", r)
	}
}

// isolateAgentStores points every on-disk agent store at empty temp dirs so Run never scans the real home.
func isolateAgentStores(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// readAgyFixture loads testdata/agy-usage.json: a noise line before the real agy JSON payload.
func readAgyFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "agy-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stubAgy replaces the agy exec seam for the duration of the test.
func stubAgy(t *testing.T, fn func(context.Context) ([]byte, error)) {
	t.Helper()
	orig := agyUsageCmd
	agyUsageCmd = fn
	t.Cleanup(func() { agyUsageCmd = orig })
}

// TestAntigravityQuotasParsesFixture: the real machine-captured agy /usage payload, past a noise line and a
// disabled bucket, becomes one row per enabled bucket with the right window, used%, resets and group label.
func TestAntigravityQuotasParsesFixture(t *testing.T) {
	stubAgy(t, func(context.Context) ([]byte, error) { return readAgyFixture(t), nil })

	got := antigravityQuotas(context.Background())
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (disabled bucket skipped): %+v", len(got), got)
	}

	weeklyReset, _ := time.Parse(time.RFC3339, "2026-10-25T19:55:08Z")
	fiveHReset, _ := time.Parse(time.RFC3339, "2026-10-23T06:22:33Z")
	argonReset, _ := time.Parse(time.RFC3339, "2026-10-25T19:54:45Z")

	cases := []struct {
		window, group, resets string
		usedPct               float64
	}{
		{"weekly", "Gemini Models", weeklyReset.Local().Format("2006-01-02 15:04"), 5},
		{"5h", "Gemini Models", fiveHReset.Local().Format("2006-01-02 15:04"), 0},
		{"weekly", "Gemini 4 Argon", argonReset.Local().Format("2006-01-02 15:04"), 15},
	}
	for i, c := range cases {
		q := got[i]
		if q.Provider != "google" || q.Plan != "Antigravity" {
			t.Errorf("row %d: provider/plan = %q/%q", i, q.Provider, q.Plan)
		}
		if q.Window != c.window {
			t.Errorf("row %d: window = %q, want %q", i, q.Window, c.window)
		}
		if q.Group != c.group {
			t.Errorf("row %d: group = %q, want %q", i, q.Group, c.group)
		}
		if q.UsedPct != c.usedPct {
			t.Errorf("row %d: usedPct = %v, want %v", i, q.UsedPct, c.usedPct)
		}
		if q.Resets != c.resets {
			t.Errorf("row %d: resets = %q, want %q", i, q.Resets, c.resets)
		}
		if q.Note != "" {
			t.Errorf("row %d: unexpected note %q on a successful row", i, q.Note)
		}
	}
}

// TestAntigravityQuotasDisabledBucketOmitted: the fixture's disabled "Legacy Limit" bucket produces no row.
func TestAntigravityQuotasDisabledBucketOmitted(t *testing.T) {
	stubAgy(t, func(context.Context) ([]byte, error) { return readAgyFixture(t), nil })
	got := antigravityQuotas(context.Background())
	n := 0
	for _, q := range got {
		if q.Group == "Gemini Models" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("enabled Gemini Models rows = %d, want 2 (disabled bucket must not appear)", n)
	}
}

// TestAntigravityQuotasEmptyGroups: a SUCCESS payload with no buckets at all reports one note row, not zero rows.
func TestAntigravityQuotasEmptyGroups(t *testing.T) {
	stubAgy(t, func(context.Context) ([]byte, error) {
		return []byte(`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`), nil
	})
	got := antigravityQuotas(context.Background())
	if len(got) != 1 || got[0].Note != "agy usage reported no buckets" {
		t.Errorf("empty groups = %+v", got)
	}
}

// TestAntigravityQuotasGarbageOutput: output with no JSON object at all (and no error) is a parse failure, not a crash.
func TestAntigravityQuotasGarbageOutput(t *testing.T) {
	stubAgy(t, func(context.Context) ([]byte, error) { return []byte("Antigravity: nothing to see here\n"), nil })
	got := antigravityQuotas(context.Background())
	if len(got) != 1 || !strings.HasPrefix(got[0].Note, "agy usage failed:") {
		t.Errorf("garbage output = %+v", got)
	}
}

// TestAntigravityQuotasNonSuccessStatus: a well-formed but non-SUCCESS status is a parse failure (no numbers).
func TestAntigravityQuotasNonSuccessStatus(t *testing.T) {
	stubAgy(t, func(context.Context) ([]byte, error) {
		return []byte(`{"status":"ERROR","command":{"name":"usage","data":{"groups":[]}}}`), nil
	})
	got := antigravityQuotas(context.Background())
	if len(got) != 1 || !strings.HasPrefix(got[0].Note, "agy usage failed:") {
		t.Errorf("non-SUCCESS status = %+v", got)
	}
}

// TestAntigravityQuotasNotLoggedIn: agy's own "not logged in" text (either phrasing) short-circuits to one note row.
func TestAntigravityQuotasNotLoggedIn(t *testing.T) {
	for _, out := range []string{"error: not logged in\n", "please run agy login first\n"} {
		stubAgy(t, func(context.Context) ([]byte, error) { return []byte(out), errors.New("exit status 1") })
		got := antigravityQuotas(context.Background())
		if len(got) != 1 || got[0].Note != "agy not logged in (run agy login)" {
			t.Errorf("not-logged-in output %q = %+v", out, got)
		}
	}
}

// TestAntigravityQuotasAgyErrorLine: an AGY_ERROR: line in otherwise-SUCCESS-shaped output is still a failure.
func TestAntigravityQuotasAgyErrorLine(t *testing.T) {
	stubAgy(t, func(context.Context) ([]byte, error) {
		return []byte("AGY_ERROR: quota backend unreachable\n"), nil
	})
	got := antigravityQuotas(context.Background())
	if len(got) != 1 || !strings.Contains(got[0].Note, "AGY_ERROR: quota backend unreachable") {
		t.Errorf("AGY_ERROR line = %+v", got)
	}
}

// TestAntigravityQuotasMissingBinary: no output and an exec error with agy absent from the vendor bin dir
// (true of this checkout: no internal/paths.VendorBin("agy") target) reports "agy not installed", not a crash.
func TestAntigravityQuotasMissingBinary(t *testing.T) {
	if _, err := paths.VendorBin("agy"); err == nil {
		t.Skip("agy is installed in this checkout; the not-installed branch can't be exercised here")
	}
	stubAgy(t, func(context.Context) ([]byte, error) {
		return nil, errors.New("exec: \"agy\": executable file not found in $PATH")
	})
	got := antigravityQuotas(context.Background())
	if len(got) != 1 || got[0].Note != "agy not installed" {
		t.Errorf("missing binary = %+v", got)
	}
}

// TestPrintSubscriptionQuotasAntigravityLeftPercent: default --show left renders the fixture's weekly 95% left
// (UsedPct 5 from remaining_fraction 0.9508) with the group name as DETAIL, not blanked by Limit==0.
func TestPrintSubscriptionQuotasAntigravityLeftPercent(t *testing.T) {
	q := quota{Provider: "google", Plan: "Antigravity", Window: "weekly", UsedPct: 5, LeftPct: 95, Group: "Gemini Models"}
	var buf bytes.Buffer
	printSubscriptionQuotas(&buf, []quota{q}, "left")
	s := buf.String()
	if !strings.Contains(s, "95%") {
		t.Errorf("left mode must show 95%%, got:\n%s", s)
	}
	if !strings.Contains(s, "Gemini Models") {
		t.Errorf("detail must carry the group label, got:\n%s", s)
	}
}

// An entry with no provider labels as the profile alone, never "·profile".
func TestRowLabelEmptyProvider(t *testing.T) {
	if got := rowLabel("", "prod"); got != "prod" {
		t.Errorf("rowLabel(\"\", prod) = %q, want prod", got)
	}
}
