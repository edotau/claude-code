package usage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

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
