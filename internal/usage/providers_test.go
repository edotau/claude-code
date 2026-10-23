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
	// default --show is "left": LEFT column + "N of M credits left" detail, not the old USED format.
	for _, want := range []string{"claude-opus-5", "PROVIDER QUOTAS", "6250 of 7000 credits left", "agy not logged in (run agy login)", "API KEYS"} {
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
