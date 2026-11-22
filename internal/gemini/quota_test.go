package gemini

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

const usageSample = "Gemini Models\tWeekly Limit Remaining\t96%\t2026-11-21T00:05:28Z\n" +
	"Gemini Models\tFive Hour Limit Remaining\t95%\t2026-11-17T00:58:02Z\n" +
	"Claude and GPT models\tWeekly Limit Remaining\t99%\t2026-11-23T19:58:08Z\n" +
	"Claude and GPT models\tFive Hour Limit Remaining\t98%\t2026-11-17T00:58:08Z\n" +
	"Gemini 4 Argon\tGemini 4 Argon Weekly Limit\t0%\t2026-11-23T03:35:47Z\n"

var quotaNow0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func BenchmarkParseQuotas(b *testing.B) {
	out := []byte(usageSample)
	b.ReportAllocs()
	for b.Loop() {
		ParseQuotas(out)
	}
}

// stubUsage swaps the agy runner and clock, clears the cache and gives the test its own quota state.
func stubUsage(t *testing.T, run func(context.Context) ([]byte, error)) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldRun, oldNow := quotaRun, quotaNow
	quotaRun, quotaNow = run, func() time.Time { return quotaNow0 }
	quotaMemo.Drop()
	t.Cleanup(func() { quotaRun, quotaNow = oldRun, oldNow; quotaMemo.Drop() })
}

func fixedUsage(out string) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(out), nil }
}

func TestParseQuotasSample(t *testing.T) {
	rows := ParseQuotas([]byte("banner line\n" + usageSample + "Gemini Models\tOdd Limit\tn/a\t-\nGemini Models\tNo Reset Remaining\t40%\n"))
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6 (banner and the n/a line skipped): %+v", len(rows), rows)
	}
	argon := rows[4]
	if m, ok := argon.Model(); !ok || m != "gemini-4-argon" || argon.Remaining != 0 || !argon.Reset.Equal(time.Date(2026, 11, 23, 3, 35, 47, 0, time.UTC)) {
		t.Errorf("argon row = %+v (model %q, %v)", argon, m, ok)
	}
	if _, ok := rows[0].Model(); ok {
		t.Errorf("a pool limit named a model: %+v", rows[0])
	}
	if last := rows[5]; last.Remaining != 40 || !last.Reset.IsZero() {
		t.Errorf("percent with no reset = %+v, want 40%% and a zero reset", last)
	}
	if got := ParseQuotas([]byte("A\tB\t150%\nA\tB\t-5%\nA\tB\tNaN%\n")); len(got) != 0 {
		t.Errorf("out-of-range percents = %+v, want none", got)
	}
	if got := ParseQuotas(nil); len(got) != 0 {
		t.Errorf("empty output = %+v, want no rows", got)
	}
}

func TestQuotasCachesForAMinute(t *testing.T) {
	synctest.Test(t, testQuotasTTL)
}

func testQuotasTTL(t *testing.T) {
	calls := 0
	stubUsage(t, func(context.Context) ([]byte, error) { calls++; return []byte(usageSample), nil })
	for range 3 {
		if rows, err := Quotas(context.Background()); err != nil || len(rows) != 5 {
			t.Fatalf("Quotas = %d rows, %v", len(rows), err)
		}
	}
	if calls != 1 {
		t.Errorf("agy ran %d times inside the TTL, want 1", calls)
	}
	time.Sleep(quotaTTL + time.Second)
	if _, err := Quotas(context.Background()); err != nil || calls != 2 {
		t.Errorf("after the TTL: calls = %d, err = %v, want 2 calls", calls, err)
	}
}

func TestDiskCachedConcurrentCallersFetchOnce(t *testing.T) {
	var calls atomic.Int32
	stubUsage(t, nil)
	quotaNow = time.Now
	run := func(context.Context) ([]byte, error) { calls.Add(1); return []byte(usageSample), nil }
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if b, err := diskCached(context.Background(), run); err != nil || len(b) == 0 {
				t.Errorf("diskCached = %d bytes, %v", len(b), err)
			}
		})
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("fetch ran %d times for two concurrent callers, want 1", n)
	}
}

func TestQuotasFailureIsOneLineAndCached(t *testing.T) {
	calls := 0
	stubUsage(t, func(context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("exec: agy: not found\nsecond line")
	})
	for range 2 {
		rows, err := Quotas(context.Background())
		if len(rows) != 0 || err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), "agy usage unavailable") {
			t.Fatalf("Quotas = %+v, %v; want no rows and a one-line error", rows, err)
		}
	}
	if calls != 1 {
		t.Errorf("a failing agy ran %d times, want 1 (the failure is cached)", calls)
	}
	stubUsage(t, fixedUsage("error: not logged in\n"))
	if rows, err := Quotas(context.Background()); len(rows) != 0 || err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("unparsable output = %+v, %v; want its first line in the error", rows, err)
	}
}

func TestCheckHoldsAZeroPerModelLimit(t *testing.T) {
	stubUsage(t, fixedUsage(usageSample))
	q, err := Check(context.Background(), "gemini-4-hydrogen-medium", quotaNow0)
	if err != nil || q.HoldErr != nil {
		t.Fatalf("Check = %+v, %v", q, err)
	}
	if q.Pool != "Gemini Models" || q.Left != 95 || !q.HeldUntil.IsZero() {
		t.Errorf("hydrogen status = %+v, want the Gemini pool at 95%% and no hold", q)
	}
	want := time.Date(2026, 11, 23, 3, 35, 47, 0, time.UTC)
	st := router.LoadState()
	if !st[googleAccount].ModelDead["gemini-4-argon"].Equal(want) {
		t.Errorf("argon mark = %v, want the reset %v", st[googleAccount].ModelDead, want)
	}
	if st.ModelDead(googleAccount, "gemini-4-hydrogen", quotaNow0) {
		t.Error("hydrogen was held though only argon's own limit is at 0%")
	}
	c, _ := Check(context.Background(), "claude-sonnet-5-5-high", quotaNow0)
	if c.Pool != "Claude and GPT models" || c.Left != 98 {
		t.Errorf("claude status = %+v, want the Claude and GPT pool at its lowest limit, 98%%", c)
	}
	a, _ := Check(context.Background(), "gemini-4-argon-high", quotaNow0)
	if a.Pool != "Gemini 4 Argon" || a.Left != 0 || !a.HeldUntil.Equal(want) || !strings.Contains(a.Note(), "held until") {
		t.Errorf("argon status = %+v, note %q", a, a.Note())
	}
}

func TestCheckHoldsAZeroPool(t *testing.T) {
	out := strings.Replace(usageSample, "Gemini Models\tFive Hour Limit Remaining\t95%", "Gemini Models\tFive Hour Limit Remaining\t0%", 1)
	stubUsage(t, fixedUsage(out))
	q, _ := Check(context.Background(), "gemini-3.8-flash-high", quotaNow0)
	st := router.LoadState()
	reset := time.Date(2026, 11, 17, 0, 58, 2, 0, time.UTC)
	if !q.HeldUntil.Equal(reset) || !st[googleAccount].ModelDead["gemini-3-8-flash"].Equal(reset) {
		t.Errorf("status = %+v, marks = %v; want flash held until %v", q, st[googleAccount].ModelDead, reset)
	}
	if st.ModelDead(googleAccount, "claude-sonnet-5-5", quotaNow0) {
		t.Error("a Gemini pool at 0% held a Claude model")
	}
}

func TestCheckWarnsUnderTheThreshold(t *testing.T) {
	low := strings.Replace(usageSample, "Gemini Models\tFive Hour Limit Remaining\t95%", "Gemini Models\tFive Hour Limit Remaining\t12%", 1)
	stubUsage(t, fixedUsage(low))
	q, _ := Check(context.Background(), "gemini-4-hydrogen", quotaNow0)
	if w := q.Warning(); !strings.Contains(w, "12% left") || !strings.Contains(w, "Five Hour Limit") || strings.Contains(w, "\n") {
		t.Errorf("warning = %q, want one line naming the 12%% five-hour limit", w)
	}
	stubUsage(t, fixedUsage(usageSample))
	if q, _ = Check(context.Background(), "gemini-4-hydrogen", quotaNow0); q.Warning() != "" {
		t.Errorf("warning at 95%% = %q, want none", q.Warning())
	}
}

func TestQuotaLineFitsEightyColumns(t *testing.T) {
	for _, r := range ParseQuotas([]byte(usageSample)) {
		if n := len([]rune(r.Line())); n > 80 || !strings.Contains(r.Line(), "% left") {
			t.Errorf("Line() = %q (%d runes)", r.Line(), n)
		}
	}
}

func TestCheckHoldsEveryModelOfAZeroPool(t *testing.T) {
	for pool, want := range map[string][]string{
		"Gemini Models":         {"gemini-4-argon", "gemini-4-hydrogen", "gemini-3-8-flash"},
		"Claude and GPT models": {"claude-opus-5-5", "claude-sonnet-5-5", "gpt-oss-120b"},
	} {
		out := strings.Replace(usageSample, pool+"\tWeekly Limit Remaining\t"+map[string]string{"Gemini Models": "96", "Claude and GPT models": "99"}[pool]+"%", pool+"\tWeekly Limit Remaining\t0%", 1)
		stubUsage(t, fixedUsage(out))
		if _, err := Check(context.Background(), want[0], quotaNow0); err != nil {
			t.Fatal(err)
		}
		st := router.LoadState()
		for _, id := range want {
			if !st.ModelDead(googleAccount, id, quotaNow0) {
				t.Errorf("%s at 0%%: %s not held (marks %v)", pool, id, st[googleAccount].ModelDead)
			}
		}
		other := "claude-sonnet-5-5"
		if pool != "Gemini Models" {
			other = "gemini-4-hydrogen"
		}
		if st.ModelDead(googleAccount, other, quotaNow0) {
			t.Errorf("%s at 0%% held %s of the other pool", pool, other)
		}
	}
}

func TestUsageOutputSharesTheQuotasRead(t *testing.T) {
	calls := 0
	stubUsage(t, func(context.Context) ([]byte, error) { calls++; return []byte(usageSample), nil })
	if _, err := Quotas(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out, _ := UsageOutput(context.Background()); calls != 1 || len(out) == 0 {
		t.Errorf("agy runs = %d, raw %d bytes; want one run serving both readers", calls, len(out))
	}
}

func TestUsageTextReadsAgyJSON(t *testing.T) {
	stubUsage(t, fixedUsage(`{"status":"SUCCESS","response":"Gemini Models\tWeekly Limit Remaining\t96%\t2026-11-21T00:05:28Z\n"}`))
	if rows, err := Quotas(context.Background()); err != nil || len(rows) != 1 || rows[0].Remaining != 96 {
		t.Errorf("rows from the JSON reply = %+v, %v", rows, err)
	}
}

func TestDiskCachedSharesOneReadAcrossProcesses(t *testing.T) {
	stubUsage(t, nil)
	calls := 0
	run := func(context.Context) ([]byte, error) { calls++; return []byte(usageSample), nil }
	for range 3 {
		if out, err := diskCached(context.Background(), run); err != nil || len(ParseQuotas(usageText(out))) == 0 {
			t.Fatalf("diskCached = %q, %v", out, err)
		}
	}
	if calls != 1 {
		t.Errorf("agy runs = %d within a minute, want 1", calls)
	}
	file := filepath.Join(paths.StateDir(), "agy-usage.json")
	old := time.Now().Add(-quotaTTL - time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	_, _ = diskCached(context.Background(), run)
	if calls != 2 {
		t.Errorf("agy runs = %d after the minute, want 2", calls)
	}
}

// CheckCached serves a stale-but-bounded disk cache without running agy, and errors past the bound or with no file.
func TestCheckCachedNeverSpawnsAndBoundsAge(t *testing.T) {
	stubUsage(t, func(context.Context) ([]byte, error) { t.Fatal("agy spawned"); return nil, nil })
	if _, err := CheckCached(context.Background(), "gemini-4-argon", quotaNow0); err == nil {
		t.Fatal("no cache file: want an error")
	}
	file := filepath.Join(paths.StateDir(), "agy-usage.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil || os.WriteFile(file, []byte(usageSample), 0o600) != nil {
		t.Fatal("seed cache")
	}
	stale := quotaNow0.Add(-quotaTTL - 5*time.Minute)
	if err := os.Chtimes(file, stale, stale); err != nil {
		t.Fatal(err)
	}
	if q, err := CheckCached(context.Background(), "gemini-4-argon", quotaNow0); err != nil || q.Pool == "" {
		t.Fatalf("6 min old cache = %+v, %v, want a served read", q, err)
	}
	old := quotaNow0.Add(-quotaStaleMax - time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCached(context.Background(), "gemini-4-argon", quotaNow0); err == nil {
		t.Fatal("cache past the bound: want an error")
	}
}
