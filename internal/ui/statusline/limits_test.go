package statusline

import (
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
