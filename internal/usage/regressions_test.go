package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
)

func TestClaudeUsageRejectsMissingWindows(t *testing.T) {
	for _, body := range []string{`{}`, `{"five_hour":null,"seven_day":null}`, `{"five_hour":{"utilization":0}}`} {
		t.Run(body, func(t *testing.T) {
			validClaudeCreds(t)
			setClaudeCacheDir(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			stubClaudeOAuthURL(t, srv.URL)
			rows := claudeQuotas(context.Background())
			if len(rows) != 1 || rows[0].Note != "usage unavailable: unreadable response" {
				t.Fatalf("missing windows fabricated successful quota rows: %+v", rows)
			}
			if cache := readClaudeUsageCache(claudeUsageCachePath()); cache != nil {
				t.Fatalf("missing windows were cached: %+v", cache)
			}
		})
	}
}

func TestClaudeCacheSeparatesCredentials(t *testing.T) {
	cases := []struct {
		name, owner string
		age         time.Duration
		status      int
		wantNote    bool
	}{
		{"fresh other account", "account-a", 0, http.StatusOK, false},
		{"legacy unowned", "", 0, http.StatusOK, false},
		{"stale rate limited", "account-a", time.Hour, http.StatusTooManyRequests, true},
		{"stale server error", "account-a", time.Hour, http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validClaudeCreds(t)
			setClaudeCacheDir(t)
			writeClaudeUsageCache(claudeUsageCachePath(), claudeUsageCache{
				FetchedAt: time.Now().Add(-tc.age), Owner: tc.owner, SubscriptionType: "max",
				Body: []byte(`{"five_hour":{"utilization":95},"seven_day":{"utilization":95}}`),
			})
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"five_hour":{"utilization":10},"seven_day":{"utilization":20}}`)
			}))
			defer srv.Close()
			stubClaudeOAuthURL(t, srv.URL)
			rows := claudeQuotas(context.Background())
			if hits.Load() != 1 {
				t.Fatalf("current credentials reused another account's cache: hits=%d rows=%+v", hits.Load(), rows)
			}
			if tc.wantNote {
				if len(rows) != 1 || !strings.HasPrefix(rows[0].Note, "usage unavailable: HTTP") {
					t.Fatalf("another account's stale quota leaked into fallback: %+v", rows)
				}
				return
			}
			if len(rows) != 2 || rows[0].UsedPct != 10 || rows[1].UsedPct != 20 {
				t.Fatalf("expected the current account's live quota: %+v", rows)
			}
		})
	}
}

func TestRecordQuotasRejectsCachedWeeklyWindow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rows := []quota{
		{Provider: "anthropic", Window: "5h", UsedPct: 100, Note: "cached; HTTP 429", cached: true},
		{Provider: "anthropic", Window: "weekly", UsedPct: 40, cached: true},
	}
	if n := recordQuotas(rows, time.Now()); n != 0 {
		t.Fatalf("recorded %d accounts from stale weekly quota with no note", n)
	}
	if state := router.LoadState(); len(state) != 0 {
		t.Fatalf("cached weekly quota became a fresh router observation: %+v", state)
	}
}

func TestClaudeFallbackMarksEveryWindowCached(t *testing.T) {
	for _, status := range []int{0, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			validClaudeCreds(t)
			dir := setClaudeCacheDir(t)
			writeClaudeCache(t, dir, time.Now().Add(-time.Hour), nil, "max", readClaudeFixture(t))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			stubClaudeOAuthURL(t, srv.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if status == 0 {
				cancel()
			}
			rows := claudeQuotas(ctx)
			if len(rows) != 3 {
				t.Fatalf("lost fallback windows: %+v", rows)
			}
			for _, row := range rows {
				if !row.cached {
					t.Errorf("stale %s/%s quota is not marked cached: %+v", row.Window, row.Group, row)
				}
			}
			if n := recordQuotas(rows, time.Now()); n != 0 {
				t.Errorf("stale fallback refreshed %d router accounts", n)
			}
		})
	}
}

func TestClaudeLockFailureDoesNotFetch(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now().Add(-time.Hour), nil, "max", readClaudeFixture(t))
	if err := os.Mkdir(claudeUsageCachePath()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(readClaudeFixture(t))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)
	rows := claudeQuotas(context.Background())
	if hits.Load() != 0 {
		t.Fatalf("lock failure bypassed serialization: %d HTTP requests", hits.Load())
	}
	if len(rows) != 3 || !strings.Contains(rows[0].Note, "cache lock unavailable") {
		t.Fatalf("expected an explicitly stale cache fallback on lock failure: %+v", rows)
	}
}

func TestCodexFilesystemFailuresWarn(t *testing.T) {
	for _, kind := range []string{"directory", "rollout", "dangling rollout"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			dir := filepath.Join(home, "sessions")
			writeFile(t, filepath.Join(dir, "good.jsonl"), string(readFixture(t, "codex-session-fixture.jsonl")))
			makeUnreadableCodexFixture(t, dir, kind)
			row := codexUsage(context.Background(), time.Time{})
			if row.Tokens != 2200 || row.Sessions != 1 {
				t.Fatalf("read failure discarded the readable session: %+v", row)
			}
			if !strings.HasPrefix(row.Note, "incomplete:") {
				t.Fatalf("filesystem failure silently produced complete-looking totals: %+v", row)
			}
			var out bytes.Buffer
			printAgentsSection(&out, []agentUsage{row}, 1)
			if !strings.Contains(out.String(), row.Note) {
				t.Fatalf("partial totals warning missing from text report: %s", out.String())
			}
		})
	}
}

func makeUnreadableCodexFixture(t *testing.T, dir, kind string) {
	t.Helper()
	bad := filepath.Join(dir, "bad.jsonl")
	if kind == "dangling rollout" {
		if err := os.Symlink(filepath.Join(dir, "missing"), bad); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return
	}
	if kind == "directory" {
		bad = filepath.Join(dir, "unreadable")
		writeFile(t, filepath.Join(bad, "rollout.jsonl"), "{}")
	} else {
		writeFile(t, bad, "{}")
	}
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(bad, 0o700); err != nil {
			t.Error(err)
		}
	})
	f, err := os.Open(bad)
	if err == nil {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		t.Skip("filesystem permits opening mode-000 entries")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected a permission error, got %v", err)
	}
}

func TestCodexUsageHonorsExpiredBudget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeFile(t, filepath.Join(home, "sessions", "rollout.jsonl"), string(readFixture(t, "codex-session-fixture.jsonl")))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	row := codexUsage(ctx, time.Time{})
	if row.Tokens != 0 || row.Sessions != 0 {
		t.Fatalf("Codex scanned sessions after the shared budget expired: %+v", row)
	}
	if !strings.Contains(row.Note, context.DeadlineExceeded.Error()) {
		t.Fatalf("expired budget did not mark totals incomplete: %+v", row)
	}
}

func TestMalformedBudgetWarnsAndRefetches(t *testing.T) {
	var hits atomic.Int32
	p := budgetServer(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"budget_spend_statuses":[{"name":"exceeded","alert_spend_statuses":[{"scope_type":"PER_USER","time_period":"MONTH","spend":"","threshold":"100","percent_consumed":"110"}]}]}`)
	})
	now := time.Now()
	for _, at := range []time.Time{now, now.Add(time.Minute)} {
		rows := budgetQuotas(context.Background(), p, at)
		if len(rows) != 1 || !strings.Contains(rows[0].Note, "malformed budget row (exceeded)") {
			t.Fatalf("malformed exceeded budget silently disappeared: %+v", rows)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("malformed budget cached instead of retried: requests=%d", hits.Load())
	}
	if cache := readBudgetCache(budgetCachePath()); cache != nil {
		t.Fatalf("malformed budget was cached: %+v", cache)
	}
}

func TestNewestModTimePropagatesInfoError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "transcript.jsonl"), "{}")
	if err := os.Chmod(dir, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("fixture must allow ReadDir before Info fails: %v", err)
	}
	if _, err := entries[0].Info(); !errors.Is(err, os.ErrPermission) {
		t.Skipf("filesystem does not enforce directory search permissions: %v", err)
	}
	newest, err := newestModTime(dir)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "walk "+dir) {
		t.Fatalf("DirEntry.Info error was swallowed: newest=%v err=%v", newest, err)
	}
}

type partialReportWriter struct {
	writes int
	err    error
}

func (w *partialReportWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		return len(p), nil
	}
	return 0, w.err
}

func isolatedUsageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "HOME", "CODEX_HOME", "COPILOT_HOME", "XDG_DATA_HOME"} {
		t.Setenv(env, dir)
	}
	t.Setenv("OPENCODE_DB", ":memory:")
	return dir
}

func TestUsageTextReportsWriteFailure(t *testing.T) {
	dir := isolatedUsageDir(t)
	writeUsageFixture(t, dir, "project", "session", time.Minute,
		usageRecordLine("model", 7, 3, recentTS(time.Minute)))
	w := &partialReportWriter{err: errors.New("report destination closed")}
	var errw bytes.Buffer
	code := Run([]string{"--session", "session", "--providers=false"}, dir, w, &errw)
	if code != 1 || !strings.Contains(errw.String(), "usage: write output: report destination closed") {
		t.Fatalf("partial report exited successfully or hid write error: exit=%d stderr=%s", code, &errw)
	}
}

func TestWorkflowTextReportsWriteFailure(t *testing.T) {
	dir := isolatedUsageDir(t)
	writeWorkflowAgentFixture(t, dir, "project", "session", "wf_recent", "a", "Build", time.Minute,
		usageRecordLine("model", 7, 3, recentTS(time.Minute)))
	w := &partialReportWriter{err: errors.New("workflow destination closed")}
	var errw bytes.Buffer
	code := Run([]string{"--workflows", "--session", "session"}, dir, w, &errw)
	if code != 1 || !strings.Contains(errw.String(), "usage: write output: workflow destination closed") {
		t.Fatalf("partial workflow report exited successfully or hid write error: exit=%d stderr=%s", code, &errw)
	}
}

func TestWorkflowDaysFiltersResponseHistory(t *testing.T) {
	dir := isolatedUsageDir(t)
	old := usageRecordLine("expired-model", 123456, 7890, recentTS(10*24*time.Hour))
	writeWorkflowAgentFixture(t, dir, "project", "session", "wf_expired", "a", "Build", time.Minute, old)
	writeWorkflowAgentFixture(t, dir, "project", "session", "wf_mixed", "b", "Build", time.Minute, old,
		usageRecordLine("recent-model", 7, 3, recentTS(time.Minute)))
	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--days", "1"}, dir, &out, &errw); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &errw)
	}
	text := out.String()
	if strings.Contains(text, "wf_expired") || strings.Contains(text, "expired-model") {
		t.Fatalf("recent filesystem mtimes included expired response history: %s", text)
	}
	if !strings.Contains(text, "recent-model") || !strings.Contains(text, "workflow runs: 1   agent transcripts scanned: 1") {
		t.Fatalf("in-window history was lost: %s", text)
	}
	out.Reset()
	if code := Run([]string{"--workflows", "--session", "session"}, dir, &out, &errw); code != 0 {
		t.Fatalf("session exit=%d stderr=%s", code, &errw)
	}
	if !strings.Contains(out.String(), "expired-model") || !strings.Contains(out.String(), "wf_expired") {
		t.Fatalf("explicit session unexpectedly applied a days cutoff: %s", &out)
	}
}

func TestNaNWarningThresholdFallsBack(t *testing.T) {
	t.Setenv("HARNESS_USAGE_WARN_PCT", "NaN")
	rows := []quota{{Provider: "anthropic", Window: "5h", UsedPct: 90}}
	setStates(rows, warnThreshold())
	if rows[0].State != "warn" || warnThreshold() != 80 {
		t.Fatalf("NaN disabled quota warnings: threshold=%v rows=%+v", warnThreshold(), rows)
	}
}
