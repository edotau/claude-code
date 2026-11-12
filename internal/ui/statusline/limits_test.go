package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"

	"github.com/edotau/claude-code/internal/paths"
)

func writeLimitsCache(t *testing.T, fetchedAt time.Time, fiveUtil, sevenUtil float64, fiveResets, sevenResets time.Time) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"five_hour":{"utilization":%g,"resets_at":%q},"seven_day":{"utilization":%g,"resets_at":%q}}`,
		fiveUtil, fiveResets.UTC().Format(time.RFC3339), sevenUtil, sevenResets.UTC().Format(time.RFC3339))
	raw := fmt.Sprintf(`{"schema":1,"fetched_at":%q,"body":%s}`, fetchedAt.UTC().Format(time.RFC3339Nano), body)
	if err := os.WriteFile(filepath.Join(dir, "claude-oauth-usage.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Live payload rate_limits, else a fresh usage cache, render only "7d:N%"; expired, stale or no source renders nothing.
func TestLimitsSegmentSources(t *testing.T) {
	future, past := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	limits := func(reset int64) string {
		return fmt.Sprintf(`,"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},`+
			`"seven_day":{"used_percentage":41.2,"resets_at":%d}}`, reset, reset)
	}
	for _, tc := range []struct {
		name, extra string
		cacheAge    time.Duration // < 0: no cache
		want        string
	}{
		{"payload", limits(future), -1, "7d:41%"},
		{"payload window expired", limits(past), -1, ""},
		{"cache", "", 0, "7d:41%"},
		{"stale cache", "", 7 * time.Hour, ""},
		{"neither", "", -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			if tc.cacheAge >= 0 {
				reset := time.Now().Add(24 * time.Hour)
				writeLimitsCache(t, time.Now().Add(-tc.cacheAge), 23, 41, reset, reset)
			}
			got := plain(renderStatusline([]byte(`{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"}`+tc.extra+`}`), "minimal"))
			if strings.Contains(got, "5h:") || strings.Contains(got, "7d:") != (tc.want != "") || !strings.Contains(got, tc.want) {
				t.Errorf("limits = %q, want only %q", got, tc.want)
			}
		})
	}
}

// tokenLeft boundaries: expired at 0, "<1m" under a minute, whole minutes, hours and minutes, then whole days.
func TestTokenLeftBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for left, want := range map[time.Duration]string{
		-time.Second:                    "u2m:exp",
		0:                               "u2m:exp",
		time.Second:                     "u2m:<1m",
		59 * time.Second:                "u2m:<1m",
		time.Minute:                     "u2m:1m",
		42 * time.Minute:                "u2m:42m",
		59*time.Minute + 59*time.Second: "u2m:59m",
		time.Hour:                       "u2m:1h00m",
		65 * time.Minute:                "u2m:1h05m",
		23*time.Hour + 59*time.Minute:   "u2m:23h59m",
		24 * time.Hour:                  "u2m:1d",
		89*24*time.Hour + time.Hour:     "u2m:89d",
	} {
		if got := tokenLeft("u2m", now.Add(left), now); got != want {
			t.Errorf("tokenLeft(+%v) = %q, want %q", left, got, want)
		}
	}
}

// Only the anthropic cli (ultra) session shows 5h/7d; every other one shows its token time left or nothing.
func TestLimitsSegmentTokenVsPassthrough(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	payload := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`,
		future, future)
	var in input
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Providers["dbx-u2m"] = &providers.Provider{Name: "dbx-u2m", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthOAuth}}
	reg.Providers["dbx-m2m"] = &providers.Provider{Name: "dbx-m2m", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthM2M}}
	prev := registry
	t.Cleanup(func() { registry = prev })
	registry = func() *providers.Registry { return reg }
	writeTokenExpiry(t, "dbx-u2m", time.Now().Add(42*time.Minute+30*time.Second))
	writeTokenExpiry(t, "dbx-m2m", time.Now().Add(65*time.Minute+30*time.Second))
	reg.Providers["dbx-pat"] = &providers.Provider{Name: "dbx-pat", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthPAT}}
	writeTokenExpiry(t, "dbx-pat", time.Now().Add(30*time.Minute+30*time.Second))
	reg.Providers["dbx-key"] = &providers.Provider{Name: "dbx-key", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthKey, Command: "x"}}
	writeTokenExpiry(t, "dbx-key", time.Now().Add(42*time.Minute+30*time.Second))
	writeTokenExpiry(t, "ultra", time.Now().Add(42*time.Minute))

	for session, want := range map[string]string{
		"dbx-u2m": "u2m:42m", "dbx-m2m": "m2m:1h05m", "dbx-pat": "pat:30m", "dbx-key": "u2m:42m", // token time left
		"openai": "", "no-such-provider": "", // no record, no subscription
		"ultra": "7d:41%", // a subscription shows 5h/7d only, even with an expiry record
	} {
		got := authSegment(session)
		if onSubscription(session) {
			got = limitsSegment(in)
		}
		if got != want {
			t.Errorf("segment(%s) = %q, want %q", session, got, want)
		}
	}
}

// writeTokenExpiry plants the {"exp":N} record providers.TokenExpiry reads, without minting a credential.
func writeTokenExpiry(t *testing.T, name string, exp time.Time) {
	t.Helper()
	writeTokenExpiryMethod(t, name, exp, "")
}

// writeTokenExpiryMethod plants an expiry record tagged with the method that minted it ("" omits the tag).
func writeTokenExpiryMethod(t *testing.T, name string, exp time.Time, method string) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(fmt.Sprintf(`{"schema":2,"exp":%d,"method":%q}`, exp.Unix(), method)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// useSessions swaps the statusline registry for one holding the given profile rows.
func useSessions(t *testing.T, rows ...*providers.Provider) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		reg.Providers[p.Name] = p
	}
	prev := registry
	t.Cleanup(func() { registry = prev })
	registry = func() *providers.Registry { return reg }
}

// An oauth row served by its paired SP labels the countdown from the record's method, not the row's auth.
func TestLimitsSegmentLabelsFromRecordedMethod(t *testing.T) {
	hermetic(t)
	useSessions(t, &providers.Provider{Name: "dbx-sp", Provider: "databricks", Auth: providers.Auth{Type: providers.AuthOAuth}})
	writeTokenExpiryMethod(t, "dbx-sp", time.Now().Add(42*time.Minute+30*time.Second), "m2m")
	if got := authSegment("dbx-sp"); got != "m2m:42m" {
		t.Errorf("SP-minted record on an oauth row: %q, want m2m:42m", got)
	}
}
