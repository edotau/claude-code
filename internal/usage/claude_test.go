package usage

import (
	"context"
	"encoding/json"
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
)

// Mirrors antigravity_test.go's seam-stub style against claude.go's real seams: claudeOAuthURL (a var,
// pointed at an httptest.Server), claudeKeychainCmd (the keychain exec seam), and the CLAUDE_CONFIG_DIR
// file fallback claudeCredentials falls to when the keychain misses.
// readClaudeFixture loads testdata/claude-oauth-usage.json: the real Claude.ai OAuth usage payload.
func readClaudeFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude-oauth-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stubClaudeKeychain replaces the keychain exec seam for the duration of the test.
func stubClaudeKeychain(t *testing.T, fn func(context.Context, ...string) ([]byte, error)) {
	t.Helper()
	orig := claudeKeychainCmd
	claudeKeychainCmd = fn
	t.Cleanup(func() { claudeKeychainCmd = orig })
}

// stubClaudeOAuthURL points the usage-HTTP seam at a test server for the duration of the test.
func stubClaudeOAuthURL(t *testing.T, url string) {
	t.Helper()
	orig := claudeOAuthURL
	claudeOAuthURL = url
	t.Cleanup(func() { claudeOAuthURL = orig })
}

// claudeCredentialJSON builds a `claudeAiOauth` payload with the given expiry (ms epoch), token and plan.
func claudeCredentialJSON(token string, expiresAtMs int64, subscriptionType string) []byte {
	return []byte(`{"claudeAiOauth":{"accessToken":"` + token + `","expiresAt":` +
		strconv.FormatInt(expiresAtMs, 10) + `,"subscriptionType":"` + subscriptionType + `"}}`)
}

// keychainMiss always errors, matching a keychain with no matching entry under any service/account.
func keychainMiss(context.Context, ...string) ([]byte, error) {
	return nil, errors.New("item not found")
}

// assertClaudeFixtureRows checks each of the fixture's three rows: window, group, used %, UTC and local resets, no note.
func assertClaudeFixtureRows(t *testing.T, got []quota) {
	t.Helper()
	fiveHReset, _ := time.Parse(time.RFC3339, "2026-10-24T05:29:59.908556+00:00")
	weeklyReset, _ := time.Parse(time.RFC3339, "2026-10-26T15:59:59.908572+00:00")
	fableReset, _ := time.Parse(time.RFC3339, "2026-10-26T15:59:59.908747+00:00")

	cases := []struct {
		window, group string
		usedPct       float64
		resetsAtUTC   string
		resetsLocal   string
	}{
		{"5h", "", 24, fiveHReset.UTC().Format(time.RFC3339), fiveHReset.Local().Format("2006-01-02 15:04")},
		{"weekly", "", 51, weeklyReset.UTC().Format(time.RFC3339), weeklyReset.Local().Format("2006-01-02 15:04")},
		{"weekly", "Fable", 18, fableReset.UTC().Format(time.RFC3339), fableReset.Local().Format("2006-01-02 15:04")},
	}
	for i, c := range cases {
		q := got[i]
		if q.Provider != "anthropic" || q.Plan != "Max" {
			t.Errorf("row %d: provider/plan = %q/%q, want anthropic/Max", i, q.Provider, q.Plan)
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
		if q.ResetsAt != c.resetsAtUTC {
			t.Errorf("row %d: resetsAt = %q, want %q (RFC3339 UTC)", i, q.ResetsAt, c.resetsAtUTC)
		}
		if q.Resets != c.resetsLocal {
			t.Errorf("row %d: resets = %q, want %q (local 2006-01-02 15:04)", i, q.Resets, c.resetsLocal)
		}
		if q.Note != "" {
			t.Errorf("row %d: unexpected note %q on a successful row", i, q.Note)
		}
	}
}

// TestClaudeQuotasParsesFixture: the real oauth-usage.json becomes three rows — 5h, weekly, and the
// Fable weekly_scoped limit — each anthropic/Max with a UTC ResetsAt and a local Resets string, and the
// bearer token sent is the one the keychain returned.
func TestClaudeQuotasParsesFixture(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(readClaudeFixture(t))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)
	setClaudeCacheDir(t) // isolate the usage cache from other tests/the real home dir

	future := time.Now().Add(24 * time.Hour).UnixMilli()
	cred := claudeCredentialJSON("test-token-123", future, "max")
	stubClaudeKeychain(t, func(context.Context, ...string) ([]byte, error) { return cred, nil })

	got := claudeQuotas(context.Background())
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(got), got)
	}
	if gotAuth != "Bearer test-token-123" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-token-123")
	}

	assertClaudeFixtureRows(t, got)
}

// TestClaudeQuotasExpiredToken: an expired OAuth token (from the CLAUDE_CONFIG_DIR file fallback, the
// keychain having missed) reports one note row and never calls the usage HTTP endpoint.
func TestClaudeQuotasExpiredToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("usage endpoint must not be called when the credential is stale")
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)
	stubClaudeKeychain(t, keychainMiss)

	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	past := time.Now().Add(-24 * time.Hour).UnixMilli()
	cred := claudeCredentialJSON("test-token-123", past, "max")
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), cred, 0o600); err != nil {
		t.Fatal(err)
	}

	got := claudeQuotas(context.Background())
	want := "usage unavailable: token stale, refresh pending (use claude)"
	if len(got) != 1 || got[0].Note != want {
		t.Errorf("expired token = %+v, want one row with Note %q", got, want)
	}
}

// TestClaudeQuotasNoCredential: the keychain misses and the CLAUDE_CONFIG_DIR file fallback also misses
// (empty temp dir, no .credentials.json); reports one note row and never calls the usage HTTP endpoint.
func TestClaudeQuotasNoCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("usage endpoint must not be called when there is no credential")
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)
	stubClaudeKeychain(t, keychainMiss)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	got := claudeQuotas(context.Background())
	want := "not logged in (run claude /login)"
	if len(got) != 1 || got[0].Note != want {
		t.Errorf("no credential = %+v, want one row with Note %q", got, want)
	}
}

// TestClaudeQuotasGarbageCredential: a stored payload that isn't valid JSON (keychain miss, file fallback
// holds garbage) is treated exactly like no credential — claudeCredentials folds both into ok=false.
func TestClaudeQuotasGarbageCredential(t *testing.T) {
	stubClaudeKeychain(t, keychainMiss)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := claudeQuotas(context.Background())
	want := "not logged in (run claude /login)"
	if len(got) != 1 || got[0].Note != want {
		t.Errorf("garbage credential = %+v, want one row with Note %q", got, want)
	}
}

// TestClaudeQuotasUsageHTTPError: a non-200 from the usage endpoint (valid credential) is one note row
// naming the status code, not a crash.
func TestClaudeQuotasUsageHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)
	setClaudeCacheDir(t) // isolate the usage cache: no prior cache, so no fallback rows
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	stubClaudeKeychain(t, func(context.Context, ...string) ([]byte, error) {
		return claudeCredentialJSON("test-token-123", future, "max"), nil
	})

	got := claudeQuotas(context.Background())
	want := "usage unavailable: HTTP 429"
	if len(got) != 1 || got[0].Note != want {
		t.Errorf("usage HTTP error = %+v, want one row with Note %q", got, want)
	}
	if got[0].Provider != "anthropic" {
		t.Errorf("usage HTTP error provider = %q, want anthropic", got[0].Provider)
	}
}

// TestParseClaudeUsageUnreadableBody: a 200 response whose body isn't valid JSON is one note row.
func TestParseClaudeUsageUnreadableBody(t *testing.T) {
	got := parseClaudeUsage([]byte("not json"), "max")
	want := "usage unavailable: unreadable response"
	if len(got) != 1 || got[0].Note != want {
		t.Errorf("unreadable body = %+v, want one row with Note %q", got, want)
	}
}

// TestParseClaudeUsageNoFableLimit: a usage body with limits that don't name "Fable" yields only the
// 5h and weekly rows — the scoped-limit loop must not fabricate a Fable row from an unrelated scope.
func TestParseClaudeUsageNoFableLimit(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"five_hour": map[string]any{"utilization": 10.0, "resets_at": "2026-10-24T05:29:59Z"},
		"seven_day": map[string]any{"utilization": 20.0, "resets_at": "2026-10-26T15:59:59Z"},
		"limits": []map[string]any{
			{"kind": "weekly_scoped", "percent": 99.0, "resets_at": "2026-10-26T15:59:59Z",
				"scope": map[string]any{"model": map[string]any{"display_name": "Sonnet"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := parseClaudeUsage(body, "max")
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (no Fable-scoped limit present): %+v", len(got), got)
	}
	for _, q := range got {
		if q.Group == "Fable" {
			t.Errorf("unexpected Fable row from a non-Fable scope: %+v", q)
		}
	}
}

// TestClaudeResets: local is "2006-01-02 15:04" in the test machine's zone, utc is RFC3339 in UTC;
// an unparseable instant yields two empty strings rather than a zero-time string.
func TestClaudeResets(t *testing.T) {
	in := "2026-10-24T05:29:59Z"
	wantUTC := "2026-10-24T05:29:59Z"
	parsed, _ := time.Parse(time.RFC3339, in)
	wantLocal := parsed.Local().Format("2006-01-02 15:04")

	local, utc := claudeResets(in)
	if local != wantLocal || utc != wantUTC {
		t.Errorf("claudeResets(%q) = (%q, %q), want (%q, %q)", in, local, utc, wantLocal, wantUTC)
	}

	local, utc = claudeResets("not-a-time")
	if local != "" || utc != "" {
		t.Errorf("claudeResets(garbage) = (%q, %q), want (\"\", \"\")", local, utc)
	}
}

// On-disk cache contract: <claudeCacheDir>/claude-oauth-usage.json, TTL from HARNESS_USAGE_CACHE_TTL (seconds, default 60).

// claudeCachePathFor is this test file's own join of dir+name, used only to read/write fixtures;
// production code must compute the identical path from claudeCacheDir.
func claudeCachePathFor(dir string) string {
	return filepath.Join(dir, claudeCacheFileName)
}

// writeClaudeCache writes a cache file matching the contract JSON shape: fetched_at, optional
// not_before, subscription_type, and body (the raw oauth/usage response bytes).
func writeClaudeCache(t *testing.T, dir string, fetchedAt time.Time, notBefore *time.Time, subscriptionType string, body []byte) {
	t.Helper()
	m := map[string]any{
		"schema":            1,
		"fetched_at":        fetchedAt.UTC().Format(time.RFC3339),
		"subscription_type": subscriptionType,
		"owner":             claudeOwner(testClaudeCredentials()),
		"body":              json.RawMessage(body),
	}
	if notBefore != nil {
		m["not_before"] = notBefore.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeCachePathFor(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readClaudeCache reads back the cache file as a loose map for assertions on fetched_at/not_before.
func readClaudeCache(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(claudeCachePathFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// setClaudeCacheDir points the cache seam at a fresh temp dir for the duration of the test.
func setClaudeCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := claudeCacheDir
	claudeCacheDir = dir
	t.Cleanup(func() { claudeCacheDir = orig })
	return dir
}

func testClaudeCredentials() claudeOAuthCreds {
	var creds claudeOAuthCreds
	creds.ClaudeAiOauth.AccessToken = "test-token-123"
	creds.ClaudeAiOauth.SubscriptionType = "max"
	creds.ClaudeAiOauth.ExpiresAt = time.Now().Add(24 * time.Hour).UnixMilli()
	return creds
}

// validClaudeCreds stubs the keychain miss + CLAUDE_CONFIG_DIR file fallback with a non-expired credential.
func validClaudeCreds(t *testing.T) {
	t.Helper()
	stubClaudeKeychain(t, keychainMiss)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	cred := claudeCredentialJSON("test-token-123", future, "max")
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), cred, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeQuotasCacheWithinTTLSkipsNetwork: a fresh cache file (fetched_at = now) within the default
// 60s TTL serves rows straight from disk; the httptest handler must never be hit.
func TestClaudeQuotasCacheWithinTTLSkipsNetwork(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now(), nil, "max", readClaudeFixture(t))

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write(readClaudeFixture(t))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	got := claudeQuotas(context.Background())
	if hit {
		t.Fatal("usage endpoint hit while cache is within TTL")
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (served from cache): %+v", len(got), got)
	}
}

// TestClaudeQuotasCacheAfterTTLRefetchesAndRewrites: a stale cache (fetched_at well past TTL) triggers
// exactly one network fetch, and the cache file on disk is rewritten with a fresh fetched_at.
func TestClaudeQuotasCacheAfterTTLRefetchesAndRewrites(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now().Add(-1*time.Hour), nil, "max", readClaudeFixture(t))

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(readClaudeFixture(t))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	before := time.Now()
	got := claudeQuotas(context.Background())
	if hits != 1 {
		t.Fatalf("network hits = %d, want 1 (cache was past TTL)", hits)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(got), got)
	}

	cache := readClaudeCache(t, dir)
	fetchedAt, err := time.Parse(time.RFC3339, cache["fetched_at"].(string))
	if err != nil {
		t.Fatalf("cache fetched_at unparseable: %v", cache["fetched_at"])
	}
	if fetchedAt.Before(before.Add(-2 * time.Second)) {
		t.Errorf("cache fetched_at = %v, want refreshed to ~now (test start %v)", fetchedAt, before)
	}
}

// TestClaudeQuotasCacheInvalidBodyKeepsLastGood: a 200 with a non-JSON body is not cached; the last good body is kept.
func TestClaudeQuotasCacheInvalidBodyKeepsLastGood(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	old := time.Now().Add(-1 * time.Hour)
	writeClaudeCache(t, dir, old, nil, "max", readClaudeFixture(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>gateway</html>"))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	got := claudeQuotas(context.Background())
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 served from the last good body: %+v", len(got), got)
	}
	fetchedAt, err := time.Parse(time.RFC3339, readClaudeCache(t, dir)["fetched_at"].(string))
	if err != nil || fetchedAt.After(old.Add(2*time.Second)) {
		t.Errorf("cache fetched_at = %v (err %v), want unchanged %v", fetchedAt, err, old)
	}
}

// stale429Setup caches a stale fixture body and serves 429 Retry-After: 141; hits counts the network requests.
func stale429Setup(t *testing.T) (dir string, hits *int) {
	t.Helper()
	validClaudeCreds(t)
	dir = setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now().Add(-1*time.Hour), nil, "max", readClaudeFixture(t))

	hits = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Retry-After", "141")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	stubClaudeOAuthURL(t, srv.URL)
	return dir, hits
}

// TestClaudeQuotasCache429ServesStaleBody: a 429 keeps the old cached body and serves rows from it with
// "cached; rate limited until" in the first row's Note.
func TestClaudeQuotasCache429ServesStaleBody(t *testing.T) {
	_, hits := stale429Setup(t)
	got := claudeQuotas(context.Background())
	if *hits != 1 {
		t.Fatalf("network hits = %d, want 1 (the 429 itself)", *hits)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (served from stale cached body): %+v", len(got), got)
	}
	if !strings.Contains(got[0].Note, "cached; rate limited until") {
		t.Errorf("row 0 Note = %q, want to contain %q", got[0].Note, "cached; rate limited until")
	}
}

// TestClaudeQuotasCache429SetsNotBefore: a 429 with Retry-After: 141 sets not_before to ~now+141s, and a second
// call before not_before elapses makes no further network request.
func TestClaudeQuotasCache429SetsNotBefore(t *testing.T) {
	dir, hits := stale429Setup(t)
	before := time.Now()
	claudeQuotas(context.Background())

	cache := readClaudeCache(t, dir)
	nb, ok := cache["not_before"].(string)
	if !ok || nb == "" {
		t.Fatalf("cache not_before missing after 429: %+v", cache)
	}
	notBefore, err := time.Parse(time.RFC3339, nb)
	if err != nil {
		t.Fatalf("cache not_before unparseable: %v", nb)
	}
	wantAround := before.Add(141 * time.Second)
	if d := notBefore.Sub(wantAround); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("not_before = %v, want ~%v (now+141s)", notBefore, wantAround)
	}

	// Second call, still within the not_before window: must not touch the network again.
	got2 := claudeQuotas(context.Background())
	if *hits != 1 {
		t.Fatalf("network hits after second call = %d, want 1 (still rate-limited until %v)", *hits, notBefore)
	}
	if len(got2) != 3 {
		t.Fatalf("second call rows = %d, want 3: %+v", len(got2), got2)
	}
}

// TestClaudeQuotasCache429NoCacheReportsHTTPStatus: a 429 with no prior cache has nothing to serve,
// so the Note names the HTTP status like the existing no-cache error path.
func TestClaudeQuotasCache429NoCacheReportsHTTPStatus(t *testing.T) {
	validClaudeCreds(t)
	setClaudeCacheDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "141")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	got := claudeQuotas(context.Background())
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 (no cache to fall back to): %+v", len(got), got)
	}
	if !strings.Contains(got[0].Note, "HTTP 429") {
		t.Errorf("Note = %q, want to contain %q", got[0].Note, "HTTP 429")
	}
}

// TestClaudeQuotasCache429NoCacheStillBacksOff: a 429 with no prior cache writes not_before, so the next call
// makes no network request.
func TestClaudeQuotasCache429NoCacheStillBacksOff(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", "141")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	claudeQuotas(context.Background())
	if nb, _ := readClaudeCache(t, dir)["not_before"].(string); nb == "" {
		t.Fatalf("not_before not written after 429 with no cache")
	}
	got := claudeQuotas(context.Background())
	if hits != 1 {
		t.Fatalf("network hits = %d, want 1 (backed off)", hits)
	}
	if len(got) != 1 || !strings.Contains(got[0].Note, "rate limited until") {
		t.Errorf("second call = %+v, want one rate-limited note", got)
	}
}

// TestClaudeQuotasCacheCorruptTreatedAsAbsent: a cache file that isn't valid JSON is treated exactly
// like no cache — claudeQuotas still fetches and returns rows, not an error row.
func TestClaudeQuotasCacheCorruptTreatedAsAbsent(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	if err := os.WriteFile(claudeCachePathFor(dir), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(readClaudeFixture(t))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	got := claudeQuotas(context.Background())
	if hits != 1 {
		t.Fatalf("network hits = %d, want 1 (corrupt cache must be treated as absent)", hits)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 after fetch: %+v", len(got), got)
	}
}

// TestClaudeQuotasCacheOtherErrorFallsBackToCache: a non-429 5xx with a cache present falls back to the
// cached body, whose first row's Note starts with "cached;".
func TestClaudeQuotasCacheOtherErrorFallsBackToCache(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now().Add(-1*time.Hour), nil, "max", readClaudeFixture(t))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	got := claudeQuotas(context.Background())
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (served from cache on 5xx): %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Note, "cached;") {
		t.Errorf("row 0 Note = %q, want prefix %q", got[0].Note, "cached;")
	}
}

// TestClaudeQuotasCacheTTLFromEnv: HARNESS_USAGE_CACHE_TTL shortens/lengthens the TTL window — a cache
// 5s old is stale under a 1s TTL (triggers a fetch) but fresh under a 60s TTL (no fetch).
func TestClaudeQuotasCacheTTLFromEnv(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now().Add(-5*time.Second), nil, "max", readClaudeFixture(t))

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(readClaudeFixture(t))
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	t.Setenv("HARNESS_USAGE_CACHE_TTL", "1")
	claudeQuotas(context.Background())
	if hits != 1 {
		t.Fatalf("network hits with TTL=1s on a 5s-old cache = %d, want 1 (stale)", hits)
	}
}

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

// TestCachedSevenDayKeychainLoginUsesOwnerSidecar: keychain-only login; CachedSevenDay serves only its own cache.
func TestCachedSevenDayKeychainLoginUsesOwnerSidecar(t *testing.T) {
	setClaudeCacheDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no .credentials.json: the keychain holds the login
	stubClaudeKeychain(t, func(context.Context, ...string) ([]byte, error) {
		return claudeCredentialJSON("login-a", time.Now().Add(time.Hour).UnixMilli(), "max"), nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(readClaudeFixture(t)) }))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	if _, _, ok := CachedSevenDay(time.Hour); ok {
		t.Fatal("served with no cache")
	}
	claudeQuotas(context.Background()) // live read as login A: writes the cache and the owner sidecar
	if got, _, ok := CachedSevenDay(time.Hour); !ok || got != 51 {
		t.Fatalf("login A's own cache = %v, %v; want 51, true", got, ok)
	}
	var b claudeOAuthCreds
	b.ClaudeAiOauth.AccessToken = "login-b"
	writeClaudeOwnerSidecar(b) // the keychain login switched to B; A's cache must not be served
	if _, _, ok := CachedSevenDay(time.Hour); ok {
		t.Fatal("another login's cache was served")
	}
}
