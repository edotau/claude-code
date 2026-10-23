package usage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain keeps every Run in this package offline: the live quota fetch is stubbed out.
func TestMain(m *testing.M) {
	fetchQuotas = func() []quota { return nil }
	os.Exit(m.Run())
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
	stub := []quota{
		{Provider: "github", Account: "me", Plan: "Pro+", Window: "monthly", UsedPct: 11, Used: 750, Limit: 7000, Resets: "2026-11-01"},
		{Provider: "google", Note: googleNote},
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
	for _, want := range []string{"claude-opus-5", "PROVIDER QUOTAS", "750 / 7000 credits", googleNote} {
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
	printQuotas(&out, qs)
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
	fetchQuotas = func() []quota { return []quota{{Provider: "google", Note: googleNote}} }
	t.Cleanup(func() { fetchQuotas = orig })
	var out, errw bytes.Buffer
	if code := Run(nil, filepath.Join(t.TempDir(), "empty"), &out, &errw); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "PROVIDER QUOTAS") {
		t.Errorf("no quota block:\n%s", out.String())
	}
}
