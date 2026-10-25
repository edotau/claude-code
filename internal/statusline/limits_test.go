package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	raw := fmt.Sprintf(`{"fetched_at":%q,"body":%s}`, fetchedAt.UTC().Format(time.RFC3339Nano), body)
	if err := os.WriteFile(filepath.Join(dir, "claude-oauth-usage.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The payload's own rate_limits wins and renders "5h:N% 7d:N%".
func TestLimitsSegmentFromPayload(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(time.Hour).Unix()
	payload := fmt.Sprintf(`{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"},
		"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`,
		future, future)
	got := plain(Render([]byte(payload), "gradient"))
	if want := "5h:23% 7d:41%"; !strings.Contains(got, want) {
		t.Errorf("want %q in %q", want, got)
	}
}

// A payload window whose reset already passed is omitted, even while the other window is fresh.
func TestLimitsSegmentPayloadWindowExpired(t *testing.T) {
	hermetic(t)
	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()
	payload := fmt.Sprintf(`{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"},
		"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`,
		past, future)
	got := plain(Render([]byte(payload), "gradient"))
	if strings.Contains(got, "5h:") {
		t.Errorf("expired window rendered: %q", got)
	}
	if !strings.Contains(got, "7d:41%") {
		t.Errorf("fresh window dropped alongside the expired one: %q", got)
	}
}

// No rate_limits on the payload falls back to the read-only usage cache.
func TestLimitsSegmentFromCache(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour)
	writeLimitsCache(t, time.Now(), 23, 41, future, future)
	got := plain(Render([]byte(runningPayload), "gradient"))
	if want := "5h:23% 7d:41%"; !strings.Contains(got, want) {
		t.Errorf("want %q in %q", want, got)
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

// tokenLeft boundaries: expired at 0, "<1m" under a minute, then whole minutes, then hours and minutes.
func TestTokenLeftBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for left, want := range map[time.Duration]string{
		-time.Second:                    "tok:exp",
		0:                               "tok:exp",
		time.Second:                     "tok:<1m",
		59 * time.Second:                "tok:<1m",
		time.Minute:                     "tok:1m",
		42 * time.Minute:                "tok:42m",
		59*time.Minute + 59*time.Second: "tok:59m",
		time.Hour:                       "tok:1h00m",
		65 * time.Minute:                "tok:1h05m",
	} {
		if got := tokenLeft(now.Add(left), now); got != want {
			t.Errorf("tokenLeft(+%v) = %q, want %q", left, got, want)
		}
	}
}

// A bearer or x-api-key provider shows its token's time left; a passthrough or unknown provider keeps 5h/7d.
func TestLimitsSegmentTokenVsPassthrough(t *testing.T) {
	hermetic(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	payload := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":23.4,"resets_at":%d},"seven_day":{"used_percentage":41.2,"resets_at":%d}}}`,
		future, future)
	var in Input
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		t.Fatal(err)
	}
	writeTokenExpiry(t, "openai", time.Now().Add(42*time.Minute+30*time.Second))

	if got := limitsSegment(in, "openai"); got != "tok:42m" {
		t.Errorf("bearer provider with a record: %q, want tok:42m", got)
	}
	if got := limitsSegment(in, "anthropic"); got != "" {
		t.Errorf("x-api-key provider without a record: %q, want empty", got)
	}
	if got := limitsSegment(in, "subscription"); got != "5h:23% 7d:41%" {
		t.Errorf("passthrough provider: %q, want 5h/7d", got)
	}
	if got := limitsSegment(in, "no-such-provider"); got != "5h:23% 7d:41%" {
		t.Errorf("unknown provider: %q, want 5h/7d", got)
	}
}

// writeTokenExpiry plants the {"exp":N} record providers.TokenExpiry reads, without minting a credential.
func writeTokenExpiry(t *testing.T, name string, exp time.Time) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())), 0o600); err != nil {
		t.Fatal(err)
	}
}
