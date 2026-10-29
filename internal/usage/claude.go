// claude.go — live Claude.ai subscription quota (5h/weekly/Fable) via the OAuth usage endpoint.
package usage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
		switch runtime.GOOS {
		case "darwin":
			if out, err := claudeKeychainCmd(ctx, "find-generic-password", "-s", svc, "-a", account, "-w"); err == nil {
				if v := strings.TrimSpace(string(out)); v != "" {
					return v
				}
			}
			if out, err := claudeKeychainCmd(ctx, "find-generic-password", "-s", svc, "-w"); err == nil {
				if v := strings.TrimSpace(string(out)); v != "" {
					return v
				}
			}
		case "linux":
			if out, err := claudeKeychainCmd(ctx, "lookup", "service", svc); err == nil {
				if v := strings.TrimSpace(string(out)); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// claudeCredentialsPath is the file fallback: CLAUDE_CONFIG_DIR/.credentials.json, else ~/.claude/.credentials.json.
func claudeCredentialsPath() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".claude")
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
	FiveHour struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay struct {
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

// claudeResets parses an RFC3339 instant into formatAgyReset's local display plus a normalized UTC instant.
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
	Body             json.RawMessage `json:"body"`
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
	if !decodeUsageCache(raw, &c) || len(c.Body) == 0 {
		return nil
	}
	return &c
}

// writeClaudeUsageCache persists c to path (0600 in a 0700 dir); best-effort, a write failure never fails the verb.
func writeClaudeUsageCache(path string, c claudeUsageCache) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	c.Schema = usageCacheSchema
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = paths.AtomicWrite(path, data, 0o600)
}

// claudeRetryAfter parses Retry-After (seconds or an HTTP-date); 120s default when absent or unparsable.
func claudeRetryAfter(v string, now time.Time) time.Duration {
	const defaultCooldown = 120 * time.Second
	v = strings.TrimSpace(v)
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return defaultCooldown
}

// claudeCachedOrNote serves cache's rows with a "cached; <reason>" note on the first row when cache holds a
// body, else the plain note — the shared fallback for every error path once a prior fetch may exist.
func claudeCachedOrNote(cache *claudeUsageCache, reason, note string) []quota {
	if cache == nil || len(cache.Body) == 0 {
		return []quota{{Provider: "anthropic", Note: note}}
	}
	rows := parseClaudeUsage(cache.Body, cache.SubscriptionType)
	if len(rows) > 0 {
		rows[0].Note = "cached; " + reason
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
	if creds.ClaudeAiOauth.ExpiresAt > 0 && time.Now().UnixMilli() > creds.ClaudeAiOauth.ExpiresAt {
		return []quota{{Provider: "anthropic", Note: "token expired (run claude /login)"}}
	}

	path := claudeUsageCachePath()
	cache := readClaudeUsageCache(path)
	now := claudeNow()

	if cache != nil && now.Sub(cache.FetchedAt) < claudeUsageCacheTTL() {
		return parseClaudeUsage(cache.Body, cache.SubscriptionType)
	}
	if cache != nil && cache.NotBefore != nil && now.Before(*cache.NotBefore) {
		return claudeCachedOrNote(cache, "rate limited until "+cache.NotBefore.Local().Format("15:04"), "")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeOAuthURL, nil)
	if err != nil {
		return claudeCachedOrNote(cache, firstLine(nil, err), "usage unavailable: "+firstLine(nil, err))
	}
	req.Header.Set("Authorization", "Bearer "+creds.ClaudeAiOauth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-code/2.1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return claudeCachedOrNote(cache, firstLine(nil, err), "usage unavailable: "+firstLine(nil, err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		writeClaudeUsageCache(path, claudeUsageCache{
			FetchedAt: now, SubscriptionType: creds.ClaudeAiOauth.SubscriptionType, Body: body,
		})
		return parseClaudeUsage(body, creds.ClaudeAiOauth.SubscriptionType)
	case http.StatusTooManyRequests:
		notBefore := now.Add(claudeRetryAfter(resp.Header.Get("Retry-After"), now))
		note := fmt.Sprintf("usage unavailable: HTTP %d", resp.StatusCode)
		if cache == nil {
			return []quota{{Provider: "anthropic", Note: note}}
		}
		cache.NotBefore = &notBefore
		writeClaudeUsageCache(path, *cache)
		return claudeCachedOrNote(cache, "rate limited until "+notBefore.Local().Format("15:04"), note)
	default:
		return claudeCachedOrNote(cache, fmt.Sprintf("HTTP %d", resp.StatusCode),
			fmt.Sprintf("usage unavailable: HTTP %d", resp.StatusCode))
	}
}

// parseClaudeUsage turns an oauth/usage body into the 5h, weekly, and Fable-weekly rows.
func parseClaudeUsage(body []byte, subscriptionType string) []quota {
	var u claudeUsageResponse
	if err := json.Unmarshal(body, &u); err != nil {
		return []quota{{Provider: "anthropic", Note: "usage unavailable: unreadable response"}}
	}
	plan := title(subscriptionType)
	fiveLocal, fiveUTC := claudeResets(u.FiveHour.ResetsAt)
	weekLocal, weekUTC := claudeResets(u.SevenDay.ResetsAt)
	out := []quota{
		{Provider: "anthropic", Account: "-", Plan: plan, Window: "5h",
			UsedPct: u.FiveHour.Utilization, Resets: fiveLocal, ResetsAt: fiveUTC},
		{Provider: "anthropic", Account: "-", Plan: plan, Window: "weekly",
			UsedPct: u.SevenDay.Utilization, Resets: weekLocal, ResetsAt: weekUTC},
	}
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
