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
	got := plain(Render([]byte(payload), "minimal"))
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
	got := plain(Render([]byte(payload), "minimal"))
	if strings.Contains(got, "7d:") {
		t.Errorf("expired window rendered: %q", got)
	}
}

// No rate_limits on the payload falls back to the read-only usage cache.
func TestLimitsSegmentFromCache(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	writeLimitsCache(t, time.Now(), 23, 41, future, future)
	got := plain(Render([]byte(runningPayload), "minimal"))
	if want := "7d:41%"; !strings.Contains(got, want) || strings.Contains(got, "5h:") {
		t.Errorf("want only %q in %q", want, got)
	}
}

// A cache older than 6h is ignored outright, even though its windows have not reset.
func TestLimitsSegmentStaleCacheIgnored(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	writeLimitsCache(t, time.Now().Add(-7*time.Hour), 23, 41, future, future)
	got := plain(Render([]byte(runningPayload), "minimal"))
	if strings.Contains(got, "5h:") || strings.Contains(got, "7d:") {
		t.Errorf("stale cache used: %q", got)
	}
}

// Neither the payload nor a usable cache leaves the segment empty, not a stray separator.
func TestLimitsSegmentEmptyWhenNeitherSource(t *testing.T) {
	hermetic(t)
	got := plain(Render([]byte(runningPayload), "minimal"))
	if strings.Contains(got, "5h:") || strings.Contains(got, "7d:") {
		t.Errorf("limits rendered with no source: %q", got)
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
	reg.Providers["dbx-pat"] = &providers.Provider{Name: "dbx-pat", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthPAT}}
	writeTokenExpiry(t, "dbx-pat", time.Now().Add(30*time.Minute+30*time.Second))
	reg.Providers["dbx-key"] = &providers.Provider{Name: "dbx-key", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthKey, Command: "x"}}
	writeTokenExpiry(t, "dbx-key", time.Now().Add(42*time.Minute+30*time.Second))
	writeTokenExpiry(t, "ultra", time.Now().Add(42*time.Minute))

	if got := limitsSegment(in, "dbx-u2m"); got != "u2m:42m" {
		t.Errorf("oauth profile with a record: %q, want u2m:42m", got)
	}
	if got := limitsSegment(in, "dbx-m2m"); got != "m2m:1h05m" {
		t.Errorf("m2m profile with a record: %q, want m2m:1h05m", got)
	}
	if got := limitsSegment(in, "dbx-pat"); got != "pat:30m" {
		t.Errorf("pat profile with a record: %q, want pat:30m", got)
	}
	if got := limitsSegment(in, "dbx-key"); got != "u2m:42m" {
		t.Errorf("key+command profile with a record: %q, want u2m:42m", got)
	}
	if got := limitsSegment(in, "openai"); got != "" {
		t.Errorf("non-subscription profile without a record: %q, want empty", got)
	}
	if got := limitsSegment(in, "ultra"); got != "7d:41%" {
		t.Errorf("subscription session: %q, want 5h/7d only, even with an expiry record", got)
	}
	if got := limitsSegment(in, "no-such-provider"); got != "" {
		t.Errorf("unknown provider: %q, want empty", got)
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

// A databricks key+command session never shows 5h/7d from a fresh subscription cache; with a record it shows u2m, never tok.
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
	got := limitsSegment(Input{}, "dbx-key")
	if got != "u2m:42m" || strings.Contains(got, "tok") {
		t.Errorf("with record: %q, want u2m:42m", got)
	}
}

// An anthropic cli (ultra) session shows 7d from a fresh cache and nothing from a stale one.
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

// An oauth row served by its paired SP labels the countdown from the record's method, not the row's auth.
func TestLimitsSegmentLabelsFromRecordedMethod(t *testing.T) {
	hermetic(t)
	useSessions(t, &providers.Provider{Name: "dbx-sp", Provider: "databricks", Auth: providers.Auth{Type: providers.AuthOAuth}})
	writeTokenExpiryMethod(t, "dbx-sp", time.Now().Add(42*time.Minute+30*time.Second), "m2m")
	if got := limitsSegment(Input{}, "dbx-sp"); got != "m2m:42m" {
		t.Errorf("SP-minted record on an oauth row: %q, want m2m:42m", got)
	}
}
