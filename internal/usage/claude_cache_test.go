package usage

// Tests the on-disk cache the implementer is adding to claude.go: a JSON file under a cache dir
// (contract path: state/cache/claude-oauth-usage.json under the harness state dir) with fields
// {fetched_at, not_before?, subscription_type, body}, TTL from HARNESS_USAGE_CACHE_TTL (seconds,
// default 60).
//
// GUESSED SEAMS (not yet landed; align on "landed"):
//   - var claudeCacheDir string      — the directory holding the cache file; tests set it to t.TempDir().
//   - const claudeCacheFileName      — "claude-oauth-usage.json", joined onto claudeCacheDir.
//   - HARNESS_USAGE_CACHE_TTL         — env var read directly by claudeQuotas/claudeQuotasCached, seconds.
//
// If claudeQuotas instead takes an explicit dir/cache-path parameter, or the var/file-name differ,
// these tests will fail to compile or fail at runtime — update the names in one pass, keep the
// assertions (network-hit counts, Note substrings, not_before windowing, corrupt-cache fallback).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		"fetched_at":        fetchedAt.UTC().Format(time.RFC3339),
		"subscription_type": subscriptionType,
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

// TestClaudeQuotasCache429SetsNotBeforeAndServesStaleBody: a 429 with Retry-After: 141 sets not_before
// to ~now+141s, keeps the old cached body, serves rows from it with "cached; rate limited until" in the
// first row's Note, and a second call before not_before elapses makes no further network request.
func TestClaudeQuotasCache429SetsNotBeforeAndServesStaleBody(t *testing.T) {
	validClaudeCreds(t)
	dir := setClaudeCacheDir(t)
	writeClaudeCache(t, dir, time.Now().Add(-1*time.Hour), nil, "max", readClaudeFixture(t))

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", "141")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	stubClaudeOAuthURL(t, srv.URL)

	before := time.Now()
	got := claudeQuotas(context.Background())
	if hits != 1 {
		t.Fatalf("network hits = %d, want 1 (the 429 itself)", hits)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (served from stale cached body): %+v", len(got), got)
	}
	if !strings.Contains(got[0].Note, "cached; rate limited until") {
		t.Errorf("row 0 Note = %q, want to contain %q", got[0].Note, "cached; rate limited until")
	}

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
	if hits != 1 {
		t.Fatalf("network hits after second call = %d, want 1 (still rate-limited until %v)", hits, notBefore)
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
