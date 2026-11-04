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

// The payload's own rate_limits wins and renders only "7d:N%".
func TestLimitsSegmentFromPayload(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(time.Hour).Unix()
	payload := fmt.Sprintf(`{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"},
		"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`,
		future, future)
	got := plain(Render([]byte(payload), "gradient"))
	if want := "7d:41%"; !strings.Contains(got, want) || strings.Contains(got, "5h:") {
		t.Errorf("want only %q in %q", want, got)
	}
}

// A payload 7d window whose reset already passed is omitted.
func TestLimitsSegmentPayloadWindowExpired(t *testing.T) {
	hermetic(t)
	past := time.Now().Add(-time.Hour).Unix()
	payload := fmt.Sprintf(`{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"},
		"rate_limits":{"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`, past)
	got := plain(Render([]byte(payload), "gradient"))
	if strings.Contains(got, "7d:") {
		t.Errorf("expired window rendered: %q", got)
	}
}

// No rate_limits on the payload falls back to the read-only usage cache.
func TestLimitsSegmentFromCache(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	writeLimitsCache(t, time.Now(), 23, 41, future, future)
	got := plain(Render([]byte(runningPayload), "gradient"))
	if want := "7d:41%"; !strings.Contains(got, want) || strings.Contains(got, "5h:") {
		t.Errorf("want only %q in %q", want, got)
	}
}

// A cache older than 6h is ignored outright, even though its windows have not reset.
func TestLimitsSegmentStaleCacheIgnored(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	writeLimitsCache(t, time.Now().Add(-7*time.Hour), 23, 41, future, future)
	got := plain(Render([]byte(runningPayload), "gradient"))
	if strings.Contains(got, "5h:") || strings.Contains(got, "7d:") {
		t.Errorf("stale cache used: %q", got)
	}
}

// Neither the payload nor a usable cache leaves the segment empty, not a stray separator.
func TestLimitsSegmentEmptyWhenNeitherSource(t *testing.T) {
	hermetic(t)
	got := plain(Render([]byte(runningPayload), "gradient"))
	if strings.Contains(got, "5h:") || strings.Contains(got, "7d:") {
		t.Errorf("limits rendered with no source: %q", got)
	}
}

// tokenLeft boundaries: expired at 0, "<1min" under a minute, then whole minutes, then hours and minutes.
func TestTokenLeftBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for left, want := range map[time.Duration]string{
		-time.Second:                    "u2m:exp",
		0:                               "u2m:exp",
		time.Second:                     "u2m:<1min",
		59 * time.Second:                "u2m:<1min",
		time.Minute:                     "u2m:1min",
		42 * time.Minute:                "u2m:42min",
		59*time.Minute + 59*time.Second: "u2m:59min",
		time.Hour:                       "u2m:1h00min",
		65 * time.Minute:                "u2m:1h05min",
	} {
		if got := tokenLeft("u2m", now.Add(left), now); got != want {
			t.Errorf("tokenLeft(+%v) = %q, want %q", left, got, want)
		}
	}
}

// Only the anthropic cli (max) session shows 5h/7d; every other one shows its token time left or nothing.
func TestLimitsSegmentTokenVsPassthrough(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	payload := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`,
		future, future)
	var in Input
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
	reg.Providers["dbx-key"] = &providers.Provider{Name: "dbx-key", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthKey, Command: "x"}}
	writeTokenExpiry(t, "dbx-key", time.Now().Add(42*time.Minute+30*time.Second))
	writeTokenExpiry(t, "max", time.Now().Add(42*time.Minute))

	// Databricks carries the countdown in the model cell; its auth cell stays empty.
	for name, want := range map[string]string{"dbx-u2m": "·u2m@dbx-u2m:42min", "dbx-m2m": "·m2m@dbx-m2m:1h05min",
		"dbx-key": "·u2m@dbx-key:42min"} {
		if got := databricksModel(in, name); got != want {
			t.Errorf("%s model cell: %q, want %q", name, got, want)
		}
		if got := limitsSegment(in, name); got != "" {
			t.Errorf("%s auth cell: %q, want empty", name, got)
		}
	}
	if got := limitsSegment(in, "openai"); got != "" {
		t.Errorf("non-subscription profile without a record: %q, want empty", got)
	}
	if got := limitsSegment(in, "max"); got != "7d:41%" {
		t.Errorf("subscription session: %q, want 5h/7d only, even with an expiry record", got)
	}
	if got := limitsSegment(in, "no-such-provider"); got != "" {
		t.Errorf("unknown provider: %q, want empty", got)
	}
}

// writeTokenExpiry plants the {"exp":N} record providers.TokenExpiry reads, without minting a credential.
func writeTokenExpiry(t *testing.T, name string, exp time.Time) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(fmt.Sprintf(`{"schema":1,"exp":%d}`, exp.Unix())), 0o600); err != nil {
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

// A databricks key+command session never shows 5h/7d from a fresh subscription cache; with a record its model cell shows u2m.
func TestLimitsSegmentKeyCommandIgnoresSubscriptionCache(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	writeLimitsCache(t, time.Now(), 23, 41, future, future)
	useSessions(t, &providers.Provider{Name: "dbx-key", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthKey, Command: "x"}})
	if got := limitsSegment(Input{}, "dbx-key"); got != "" {
		t.Errorf("no record, fresh cache: %q, want empty", got)
	}
	writeTokenExpiry(t, "dbx-key", time.Now().Add(42*time.Minute+30*time.Second))
	got := databricksModel(Input{}, "dbx-key")
	if got != "·u2m@dbx-key:42min" || strings.Contains(got, "7d") {
		t.Errorf("with record: %q, want ·u2m@dbx-key:42min", got)
	}
}

// An anthropic cli (max) session shows 7d from a fresh cache and nothing from a stale one.
func TestLimitsSegmentAnthropicCacheFreshVsStale(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	useSessions(t, &providers.Provider{Name: "maxx", Provider: "anthropic", Auth: providers.Auth{Type: providers.AuthCLI}})
	writeLimitsCache(t, time.Now(), 23, 41, future, future)
	if got := limitsSegment(Input{}, "maxx"); !strings.Contains(got, "7d:41%") {
		t.Errorf("fresh cache: %q, want 7d:41%%", got)
	}
	writeLimitsCache(t, time.Now().Add(-7*time.Hour), 23, 41, future, future)
	if got := limitsSegment(Input{}, "maxx"); got != "" {
		t.Errorf("stale cache: %q, want empty", got)
	}
}
