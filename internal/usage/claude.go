// claude.go — live Claude.ai subscription quota (weekly/Fable, 5h when reported) via the OAuth usage endpoint.

package usage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/network/httpx"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// claudeNow is the wall-clock seam; tests point it at a fixed instant.
var claudeNow = time.Now

// claudeOAuthURL is Claude Code's own OAuth usage endpoint; a var so tests can point it at httptest.
var claudeOAuthURL = "https://api.anthropic.com/api/oauth/usage"

// claudeKeychainCmd runs the OS secret-store lookup (security on macOS, secret-tool on Linux); tests
// replace it so none of this reaches the real keychain.
var claudeKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
	name := "security"
	if runtime.GOOS == "linux" {
		name = "secret-tool"
	}
	return exec.CommandContext(ctx, name, args...).Output()
}

var claudeAccountRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// claudeAccount is the keychain account for the credential: $USER when it's keychain-safe, else a fallback.
func claudeAccount() string {
	if u := os.Getenv("USER"); u != "" && claudeAccountRE.MatchString(u) {
		return u
	}
	return "claude-code-user"
}

// claudeServiceNames lists keychain service names to try, the CLAUDE_CONFIG_DIR-scoped one first.
func claudeServiceNames() []string {
	const base = "Claude Code-credentials"
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		return []string{base}
	}
	sum := sha256.Sum256([]byte(dir))
	return []string{base + "-" + hex.EncodeToString(sum[:])[:8], base}
}

// claudeKeychainToken tries every service/account pairing this OS supports; "" when nothing answers.
func claudeKeychainToken(ctx context.Context) string {
	account := claudeAccount()
	for _, svc := range claudeServiceNames() {
		var attempts [][]string
		switch runtime.GOOS {
		case "darwin":
			attempts = [][]string{{"find-generic-password", "-s", svc, "-a", account, "-w"}, {"find-generic-password", "-s", svc, "-w"}}
		case "linux":
			attempts = [][]string{{"lookup", "service", svc}}
		}
		for _, args := range attempts {
			if out, err := claudeKeychainCmd(ctx, args...); err == nil {
				if v := strings.TrimSpace(string(out)); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// claudeCredentialsPath is the file fallback: CLAUDE_CONFIG_DIR/.credentials.json, else the harness config dir.
func claudeCredentialsPath() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		if dir = paths.ConfigDir(); dir == "" {
			return "", errors.New("no home directory")
		}
	}
	return filepath.Join(dir, ".credentials.json"), nil
}

// claudeOAuthCreds mirrors the stored {"claudeAiOauth": {...}} payload, keychain value or file alike.
type claudeOAuthCreds struct {
	ClaudeAiOauth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// claudeCredentials resolves the OAuth payload from the keychain first, the credentials file otherwise.
func claudeCredentials(ctx context.Context) (claudeOAuthCreds, bool) {
	raw := claudeKeychainToken(ctx)
	if raw == "" {
		if path, err := claudeCredentialsPath(); err == nil {
			if body, rerr := os.ReadFile(path); rerr == nil {
				raw = string(body)
			}
		}
	}
	if raw == "" {
		return claudeOAuthCreds{}, false
	}
	var creds claudeOAuthCreds
	if err := json.Unmarshal([]byte(raw), &creds); err != nil || creds.ClaudeAiOauth.AccessToken == "" {
		return claudeOAuthCreds{}, false
	}
	return creds, true
}

// claudeUsageResponse mirrors the oauth/usage body: the two headline windows plus a scoped-limit list.
type claudeUsageResponse struct {
	FiveHour *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day"`
	Limits []struct {
		Kind     string  `json:"kind"`
		Percent  float64 `json:"percent"`
		ResetsAt string  `json:"resets_at"`
		Scope    *struct {
			Model struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

// claudeResets parses an RFC3339 instant into a local display plus a normalized UTC instant.
func claudeResets(s string) (local, utc string) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", ""
	}
	return t.Local().Format("2006-01-02 15:04"), t.UTC().Format(time.RFC3339)
}

// claudeUsageCache is the on-disk shape shared by every claude-code usage run (CLI and the VS Code poller).
type claudeUsageCache struct {
	Schema           int             `json:"schema"`
	FetchedAt        time.Time       `json:"fetched_at"`
	NotBefore        *time.Time      `json:"not_before,omitempty"`
	SubscriptionType string          `json:"subscription_type"`
	Owner            string          `json:"owner,omitempty"` // claudeOwner of the credentials that fetched Body
	Body             json.RawMessage `json:"body,omitempty"`
}

// claudeOwner fingerprints the access token so a cache is only ever served to the credentials that wrote it.
func claudeOwner(creds claudeOAuthCreds) string {
	sum := sha256.Sum256([]byte(creds.ClaudeAiOauth.AccessToken))
	return hex.EncodeToString(sum[:])
}

// ownedCache returns c only when it was written under creds; another login's cache (or an unowned one) is a miss.
func ownedCache(c *claudeUsageCache, creds claudeOAuthCreds) *claudeUsageCache {
	if c == nil || c.Owner != claudeOwner(creds) {
		return nil
	}
	return c
}

// CachedSevenDay reads the cached 7d window, no network or keychain; !ok when absent, over maxAge or another login's.
func CachedSevenDay(maxAge time.Duration) (utilization float64, resetsAt string, ok bool) {
	c := readClaudeUsageCache(claudeUsageCachePath())
	if c == nil || len(c.Body) == 0 || claudeNow().Sub(c.FetchedAt) > maxAge {
		return 0, "", false
	}
	// An unowned cache, or a login only the keychain knows (too slow to ask per render), is served.
	if c.Owner != "" {
		if cur := claudeFileOwner(); cur != "" && cur != c.Owner {
			return 0, "", false
		}
	}
	var b struct {
		SevenDay struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
	}
	if json.Unmarshal(c.Body, &b) != nil {
		return 0, "", false
	}
	return b.SevenDay.Utilization, b.SevenDay.ResetsAt, true
}

// claudeFileOwner is claudeOwner of the login in .credentials.json alone; "" when only a keychain holds it.
func claudeFileOwner() string {
	path, err := claudeCredentialsPath()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	var creds claudeOAuthCreds
	if err != nil || json.Unmarshal(raw, &creds) != nil || creds.ClaudeAiOauth.AccessToken == "" {
		return ""
	}
	return claudeOwner(creds)
}

// claudeCacheDir overrides the usage cache dir (tests); empty resolves state/harness/cache at call time.
var claudeCacheDir string

// claudeCacheFileName is the cache file's name within claudeCacheDir.
const claudeCacheFileName = "claude-oauth-usage.json"

// claudeUsageCachePath is the one cache file every run reads/writes, scoped by CLAUDE_CONFIG_DIR like StateDir.
func claudeUsageCachePath() string {
	return filepath.Join(cmp.Or(claudeCacheDir, filepath.Join(paths.StateDir(), "cache")), claudeCacheFileName)
}

// claudeUsageCacheTTL is how long a fetch stays fresh before a run hits the network again; env-overridable.
func claudeUsageCacheTTL() time.Duration {
	secs := 60
	if v := os.Getenv("HARNESS_USAGE_CACHE_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			secs = n
		}
	}
	return time.Duration(secs) * time.Second
}

// readClaudeUsageCache loads path; missing, unreadable or corrupt is absent, never an error callers must handle.
func readClaudeUsageCache(path string) *claudeUsageCache {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c claudeUsageCache
	if !schemas.DecodeOK(usageCacheFormat, raw, &c) || (len(c.Body) == 0 && c.NotBefore == nil) {
		return nil
	}
	return &c
}

// writeClaudeUsageCache persists c to path (0600 in a 0700 dir); best-effort, a write failure never fails the verb.
func writeClaudeUsageCache(path string, c claudeUsageCache) {
	c.Schema = usageCacheSchema
	_ = paths.WriteJSON(path, c)
}

// claudeRetryAfter parses Retry-After (seconds or an HTTP-date); 120s default when absent or unparsable.
func claudeRetryAfter(v string, now time.Time) time.Duration {
	const defaultCooldown = 120 * time.Second
	if after, ok := textutil.ParseRetryAfter(v, now); ok && after > 0 {
		return after
	}
	return defaultCooldown
}

// claudeCachedOrNote serves cache's rows (all marked cached) with a "cached; <reason>" note, else the plain note.
func claudeCachedOrNote(cache *claudeUsageCache, reason, note string) []quota {
	if cache == nil || len(cache.Body) == 0 {
		return []quota{{Provider: "anthropic", Note: note}}
	}
	rows := markCached(parseClaudeUsage(cache.Body, cache.SubscriptionType))
	if len(rows) == 0 || rows[0].Note != "" {
		return []quota{{Provider: "anthropic", Note: note}}
	}
	rows[0].Note = "cached; " + reason
	return rows
}

// markCached flags every row as served from the cache, so no caller records it as a fresh observation.
func markCached(rows []quota) []quota {
	for i := range rows {
		rows[i].cached = true
	}
	return rows
}

// claudeQuotas reads Claude.ai's OAuth usage endpoint: the 5h and weekly windows plus the Fable weekly cap.
// A shared on-disk cache keeps the VS Code poller and manual runs from hammering the endpoint and backs off
// on a 429 until the server's Retry-After elapses.
func claudeQuotas(ctx context.Context) []quota {
	creds, ok := claudeCredentials(ctx)
	if !ok {
		return []quota{{Provider: "anthropic", Note: "not logged in (run claude /login)"}}
	}
	path := claudeUsageCachePath()
	var rows []quota
	// The lock spans read, fetch and write, so a concurrent run reads the fresh cache instead of refetching.
	err := paths.WithFileLock(path, func() error { rows = claudeFetch(ctx, creds, path); return nil })
	if err != nil && rows == nil {
		// Unsynchronized fetches could race another run's cache write, so serve the cache without fetching.
		return claudeCachedOrNote(ownedCache(readClaudeUsageCache(path), creds), "cache lock unavailable",
			"usage unavailable: "+commandErrorLine(nil, err))
	}
	return rows
}

// claudeFetch serves the cache when fresh or rate limited, else asks the endpoint and rewrites the cache.
func claudeFetch(ctx context.Context, creds claudeOAuthCreds, path string) []quota {
	cache := ownedCache(readClaudeUsageCache(path), creds)
	now := claudeNow()

	if rows, done := claudeServeLocal(cache, creds, now); done {
		return rows
	}

	status, body, hdr, err := httpx.Do(ctx, httpx.Call{Client: http.DefaultClient, URL: claudeOAuthURL, Limit: 1 << 20,
		Header: http.Header{
			"Authorization":  {"Bearer " + creds.ClaudeAiOauth.AccessToken},
			"Anthropic-Beta": {"oauth-2025-04-20"}, "User-Agent": {"claude-code/2.1.0"},
		}})
	if status == 0 {
		return claudeCachedOrNote(cache, commandErrorLine(nil, err), "usage unavailable: "+commandErrorLine(nil, err))
	}
	if err != nil {
		reason := "read error: " + textutil.FirstLine(err.Error())
		return claudeCachedOrNote(cache, reason, "usage unavailable: "+reason)
	}
	return claudeHandleResponse(status, hdr, body, cache, creds, path, now)
}

// claudeServeLocal answers without a request: from a fresh cache, a rate-limit hold, or a stale token; done is false to fetch.
func claudeServeLocal(cache *claudeUsageCache, creds claudeOAuthCreds, now time.Time) (rows []quota, done bool) {
	if cache != nil && now.Sub(cache.FetchedAt) < claudeUsageCacheTTL() {
		return markCached(parseClaudeUsage(cache.Body, cache.SubscriptionType)), true
	}
	if cache != nil && cache.NotBefore != nil && now.Before(*cache.NotBefore) {
		return claudeCachedOrNote(cache, "rate limited until "+cache.NotBefore.Local().Format("15:04"),
			"usage unavailable: rate limited until "+cache.NotBefore.Local().Format("15:04")), true
	}
	if exp := creds.ClaudeAiOauth.ExpiresAt; exp > 0 && now.UnixMilli() > exp { // claude refreshes it on its next use
		return claudeCachedOrNote(cache, "token stale, refresh pending", "usage unavailable: token stale, refresh pending (use claude)"), true
	}
	return nil, false
}

// claudeHandleResponse maps the endpoint's status to rows, rewriting the cache on 200 and on 429.
func claudeHandleResponse(status int, hdr http.Header, body []byte, cache *claudeUsageCache, creds claudeOAuthCreds, path string, now time.Time) []quota {
	switch status {
	case http.StatusOK:
		if _, ok := claudeUsageWindows(body); !ok { // never cache a body that lacks a headline window
			return claudeCachedOrNote(cache, "unreadable response", "usage unavailable: unreadable response")
		}
		writeClaudeUsageCache(path, claudeUsageCache{
			FetchedAt: now, SubscriptionType: creds.ClaudeAiOauth.SubscriptionType, Owner: claudeOwner(creds), Body: body,
		})
		return parseClaudeUsage(body, creds.ClaudeAiOauth.SubscriptionType)
	case http.StatusTooManyRequests:
		notBefore := now.Add(claudeRetryAfter(hdr.Get("Retry-After"), now))
		note := fmt.Sprintf("usage unavailable: HTTP %d", status)
		if cache == nil {
			cache = &claudeUsageCache{SubscriptionType: creds.ClaudeAiOauth.SubscriptionType, Owner: claudeOwner(creds)}
		}
		cache.NotBefore = &notBefore
		writeClaudeUsageCache(path, *cache)
		return claudeCachedOrNote(cache, "rate limited until "+notBefore.Local().Format("15:04"), note)
	default:
		return claudeCachedOrNote(cache, fmt.Sprintf("HTTP %d", status),
			fmt.Sprintf("usage unavailable: HTTP %d", status))
	}
}

// claudePlanName is the plan name ~/.claude.json shows ("Ultra"); "" when absent.
func claudePlanName() string {
	b, err := os.ReadFile(paths.ClaudeJSON())
	if err != nil {
		return ""
	}
	var cfg struct {
		Account struct {
			PlanName string `json:"planDisplayName"`
		} `json:"oauthAccount"`
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg.Account.PlanName
}

// claudeUsageWindows decodes an oauth/usage body; the weekly window must be present, the retired 5h one may be absent.
func claudeUsageWindows(body []byte) (claudeUsageResponse, bool) {
	var u claudeUsageResponse
	if json.Unmarshal(body, &u) != nil || u.SevenDay == nil {
		return u, false
	}
	return u, true
}

// parseClaudeUsage turns an oauth/usage body into the 5h, weekly, and Fable rows; a missing window is a note.
func parseClaudeUsage(body []byte, subscriptionType string) []quota {
	u, ok := claudeUsageWindows(body)
	if !ok {
		return []quota{{Provider: "anthropic", Note: "usage unavailable: unreadable response"}}
	}
	plan := cmp.Or(claudePlanName(), textutil.Title(subscriptionType))
	var out []quota
	if u.FiveHour != nil {
		fiveLocal, fiveUTC := claudeResets(u.FiveHour.ResetsAt)
		out = append(out, quota{Provider: "anthropic", Account: "-", Plan: plan, Window: "5h",
			UsedPct: u.FiveHour.Utilization, Resets: fiveLocal, ResetsAt: fiveUTC})
	}
	weekLocal, weekUTC := claudeResets(u.SevenDay.ResetsAt)
	out = append(out, quota{Provider: "anthropic", Account: "-", Plan: plan, Window: "weekly",
		UsedPct: u.SevenDay.Utilization, Resets: weekLocal, ResetsAt: weekUTC})
	for _, l := range u.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model.DisplayName != "Fable" {
			continue
		}
		local, utc := claudeResets(l.ResetsAt)
		out = append(out, quota{Provider: "anthropic", Account: "-", Plan: plan, Window: "weekly",
			Group: "Fable", UsedPct: l.Percent, Resets: local, ResetsAt: utc})
	}
	return out
}

// usageCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const usageCacheSchema = 1

var usageCacheFormat = schemas.Register(schemas.Format{
	Name: "oauth-usage-cache", Owner: "internal/usage", Kind: schemas.Cache,
	Glob:    func() string { return claudeUsageCachePath() },
	New:     func() any { return &claudeUsageCache{} },
	Version: usageCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
