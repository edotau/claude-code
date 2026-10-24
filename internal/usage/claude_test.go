package usage

// Mirrors antigravity_test.go's seam-stub style against claude.go's real seams: claudeOAuthURL (a var,
// pointed at an httptest.Server), claudeKeychainCmd (the keychain exec seam), and the CLAUDE_CONFIG_DIR
// file fallback claudeCredentials falls to when the keychain misses.
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

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

// TestClaudeQuotasExpiredToken: an expired OAuth token (from the CLAUDE_CONFIG_DIR file fallback, the
// keychain having missed) reports one note row and never calls the usage HTTP endpoint.
func TestClaudeQuotasExpiredToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("usage endpoint must not be called when the credential is expired")
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
	want := "token expired (run claude /login)"
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

// TestFetchQuotasSeamUntouched: fetchQuotas' own network seam (providers_test.go's contract) is still
// swappable exactly as before — the claude addition composes into it without reaching past package vars.
func TestFetchQuotasSeamUntouched(t *testing.T) {
	orig := fetchQuotas
	t.Cleanup(func() { fetchQuotas = orig })
	fetchQuotas = func() []quota { return []quota{{Provider: "stub"}} }
	got := fetchQuotas()
	if len(got) != 1 || got[0].Provider != "stub" {
		t.Errorf("fetchQuotas seam not swappable as before: %+v", got)
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
