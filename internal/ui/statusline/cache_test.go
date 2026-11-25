package statusline

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

func TestStampLivesUnderStateDirAndRoundTrips(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := stampFile("tier", "k")
	if !strings.HasPrefix(p, paths.StateDir()+string(filepath.Separator)) {
		t.Fatalf("stamp %s is outside the state dir", p)
	}
	writeTimedStamp(p, "a", "b")
	if got := freshStamp(p, 1<<40, 2); len(got) != 2 || got[1] != "b" {
		t.Fatalf("freshStamp = %v", got)
	}
	if fi, err := os.Stat(p); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode = %v, %v", fi, err)
	}
}

// A planted symlink at the old tmp stamp path must not be followed: stamps live in the state dir now.
func TestWriteStampIgnoresLegacyTmpSymlink(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("k"))
	old := filepath.Join(tmp, ".claude-statusline-tier-"+strconv.Itoa(os.Getuid())+"-"+strconv.FormatUint(h.Sum64(), 16))
	if err := os.Symlink(victim, old); err != nil {
		t.Fatal(err)
	}
	p := stampFile("tier", "k")
	writeStamp(p, "a")
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim overwritten: %q", b)
	}
	di, derr := os.Stat(filepath.Dir(p))
	fi, ferr := os.Stat(p)
	if derr != nil || ferr != nil || runtime.GOOS != "windows" && (di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600) {
		t.Errorf("stamp dir/file = %v %v (%v, %v), want 0700/0600", di, fi, derr, ferr)
	}
}

func writeRunningDispatch(t *testing.T, id string, pid, owner int) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "dispatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"id": id, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"agent": "codex", "request": map[string]any{}, "status": "running", "pid": pid, "owner": owner})
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runningDispatches counts only this session's live dispatches: dead pids and other owners stay out.
func TestRunningCountsOwnLiveDispatchesOnly(t *testing.T) {
	cases := []struct {
		name  string
		pid   int
		owner func(int) int
		want  int
	}{
		{"own live", os.Getpid(), func(o int) int { return o }, 1},
		{"dead pid", 1<<22 + 12345, func(o int) int { return o }, 0}, // beyond pid_max: never alive
		{"other owner", os.Getpid(), func(o int) int { return o + 999999 }, 0},
		{"no owner", os.Getpid(), func(int) int { return 0 }, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hermetic(t)
			owner := inflight.SessionOwner()
			if owner == 0 && c.name != "own live" && c.name != "dead pid" {
				t.Skip("inflight.SessionOwner() is 0 here; owner filtering is untestable")
			}
			writeRunningDispatch(t, "20261024T000001.000-codex", c.pid, c.owner(owner))
			if got := runningDispatches(input{}); got != c.want {
				t.Errorf("dispatches = %d, want %d", got, c.want)
			}
		})
	}
}

// total gains a trailing "+" only while one of this session's dispatch dispatches runs.
func TestTotalSegmentPlusWhileDispatchRuns(t *testing.T) {
	hermetic(t)
	var in input
	if err := json.Unmarshal([]byte(`{"session_id":"s1","context_window":{"total_input_tokens":812}}`), &in); err != nil {
		t.Fatal(err)
	}
	if got := totalSegment(in); got != "812" {
		t.Fatalf("idle = %q, want 812", got)
	}
	writeRunningDispatch(t, "20261024T000002.000-codex", os.Getpid(), inflight.SessionOwner())
	// runningDispatches caches for runningTTL, so drop the stamp the idle call wrote.
	_ = os.Remove(stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath))
	if got := totalSegment(in); got != "812+" {
		t.Errorf("running = %q, want 812+", got)
	}
}

func writeLimitsCache(t *testing.T, fetchedAt time.Time, fiveUtil, sevenUtil float64, resets time.Time) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"five_hour":{"utilization":%g,"resets_at":%q},"seven_day":{"utilization":%g,"resets_at":%q}}`,
		fiveUtil, resets.UTC().Format(time.RFC3339), sevenUtil, resets.UTC().Format(time.RFC3339))
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
				writeLimitsCache(t, time.Now().Add(-tc.cacheAge), 23, 41, reset)
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
