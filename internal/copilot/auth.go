package copilot

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/usage"
	"github.com/edotau/claude-code/internal/vault"
)

// copilotTokenEnvs is copilot's documented env precedence (`copilot help environment`).
var copilotTokenEnvs = []string{providers.CopilotTokenEnv, "GH_TOKEN", "GITHUB_TOKEN"}

// CopilotAuth is the GitHub identity copilot authenticates as and where its credential comes from.
type CopilotAuth struct {
	Account string // GitHub login; "" when it needs a network lookup the caller skipped, or that failed
	Source  string // srcPinned, srcActive, an env var name, or srcCopilot
	token   string
}

// Credential sources, highest first; the gh ones resolve a token at launch with `gh auth token [-u]`.
const (
	srcPinned  = "gh (pinned)"
	srcActive  = "gh (active)"
	srcCopilot = "copilot login"
	// SrcStoredToken is a COPILOT_GITHUB_TOKEN pasted via configure (sealed in the vault).
	SrcStoredToken = providers.CopilotTokenEnv + " (stored)"
)

// copilotPinLogin is the pinned copilot profile's GitHub login ("" = unpinned: gh's active account).
func copilotPinLogin() string {
	login, _ := copilotPinLoginErr()
	return login
}

// copilotPinLoginErr is copilotPinLogin plus the providers.Load error it would swallow.
func copilotPinLoginErr() (string, error) {
	reg, err := providers.Load()
	login, _ := providers.CopilotLogin(reg)
	return login, err
}

// copilotAuthSource (no network): the pinned profile's gh login, a stored token, gh's active account, then
// copilot's own order (env token, its stored login) when gh holds no account; none when the registry can't load.
func copilotAuthSource() (CopilotAuth, bool) {
	a, ok, err := copilotAuthSourceErr()
	return a, ok && err == nil // an unreadable registry may hide a pin, so no other account is used
}

// copilotAuthSourceErr is copilotAuthSource plus the registry load error; for display, a load error falls through.
func copilotAuthSourceErr() (CopilotAuth, bool, error) {
	pin, loadErr := copilotPinLoginErr()
	a, ok := authSourceFor(pin)
	return a, ok, loadErr
}

// authSourceFor resolves the source for the pinned GitHub login pin ("" = unpinned).
func authSourceFor(pin string) (CopilotAuth, bool) {
	if pin != "" { // a pinned profile with no login falls through to gh's active account
		if slices.Contains(GHAccounts(), pin) {
			return CopilotAuth{Account: pin, Source: srcPinned}, true
		}
		return CopilotAuth{}, false // a pinned account that can't be used never falls back to another one
	}
	if tok, src, ok := vault.Get(providers.CopilotTokenEnv); ok && src != "env" {
		return CopilotAuth{Source: SrcStoredToken, token: strings.TrimSpace(tok)}, true
	}
	if login, tok := ghActiveAccountToken(); login != "" {
		return CopilotAuth{Account: login, Source: srcActive, token: tok}, true
	}
	for _, k := range copilotTokenEnvs {
		if tok := strings.TrimSpace(os.Getenv(k)); tok != "" {
			return CopilotAuth{Source: k, token: tok}, true
		}
	}
	if login := CopilotStoredLogin(); login != "" {
		return CopilotAuth{Account: login, Source: srcCopilot}, true
	}
	return CopilotAuth{}, false
}

// CopilotLaunchToken is the token a copilot launch injects (COPILOT_GITHUB_TOKEN for the CLI, GitHubToken for
// the SDK), or "" to leave copilot on its own credential. It logs the source to stderr and the audit log.
func CopilotLaunchToken() (string, error) {
	if err := copilotPinError(); err != nil {
		return "", err
	}
	a, ok := copilotAuthSource()
	if !ok {
		return "", nil
	}
	tok, how := copilotToken(a)
	if a.Source == srcPinned && tok == "" {
		return "", fmt.Errorf("copilot: no gh token for pinned GitHub user %s (`gh auth token -u` failed "+
			"or timed out); when it is signed out, run `claude-code configure github`", a.Account)
	}
	fmt.Fprintf(os.Stderr, "copilot: GitHub account %s (%s)\n", cmp.Or(a.Account, "from token"), how)
	audit.Log(audit.CopilotAuth, cmp.Or(a.Account, a.Source), how)
	return tok, nil
}

// copilotPinError is non-nil when the copilot profile's GitHub login is pinned but that user is not a gh login: the harness
// fails rather than run copilot on a stored token or another account.
func copilotPinError() error {
	pin, err := copilotPinLoginErr()
	if err != nil {
		return fmt.Errorf("copilot: cannot read the provider registry to check the pinned GitHub account "+
			"(launch refused): %w", err)
	}
	if pin == "" || slices.Contains(GHAccounts(), pin) {
		return nil
	}
	return fmt.Errorf("copilot: pinned GitHub user %s is not logged in to gh; run `claude-code configure github` "+
		"(no other account is used)", pin)
}

// RefreshAccount re-fetches a stale account for copilotUsedPct; a var so tests stay offline.
var RefreshAccount = usage.RefreshAccount

// CopilotUsedPct is a var so a test can pin a login's credit share.
var CopilotUsedPct = copilotUsedPct

// copilotUsedPct is github·<profile>'s used share from route state, refreshed through RefreshAccount past its TTL.
func copilotUsedPct(ctx context.Context, login string) (float64, bool) {
	reg, _ := providers.Load()
	key, now := "github·"+providers.GithubAccountName(reg, login), time.Now()
	e := router.LoadState()[key]
	if e.ExhaustedUntil.After(now) {
		return router.ExhaustedPct, true
	}
	if now.Sub(e.Seen) > router.StaleAfter {
		if !RefreshAccount(ctx, key) {
			return 0, false
		}
		e = router.LoadState()[key]
	}
	return e.UsedPct, !e.Seen.IsZero()
}

// copilotWorkablePct is the used credit share at which a copilot GitHub-login dispatch is no longer started.
const copilotWorkablePct = 95.0

// copilotHeadroom reports whether user is a gh login whose credits are verified under copilotWorkablePct, plus the
// notice detail naming the account and its verified share.
func copilotHeadroom(ctx context.Context, user string) (detail string, ok bool) {
	switch {
	case user == "":
		return "no pinned GitHub account", false
	case !slices.Contains(GHAccounts(), user):
		return user + " not logged in to gh", false
	}
	pct, ok := CopilotUsedPct(ctx, user)
	if !ok {
		return user + " unverified", false
	}
	return fmt.Sprintf("%s %.1f%% of credits used", user, pct), pct < copilotWorkablePct
}

// commentRE strips the // comment lines copilot writes at the top of config.json.
var commentRE = regexp.MustCompile(`(?m)^[ \t]*//.*$`)

// CopilotStoredLogin is lastLoggedInUser.login from copilot's config ($COPILOT_HOME or ~/.copilot), or "".
func CopilotStoredLogin() string {
	dir, err := paths.CopilotHome()
	if err != nil {
		dir = ".copilot"
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Last struct {
			Login string `json:"login"`
		} `json:"lastLoggedInUser"`
	}
	if json.Unmarshal(commentRE.ReplaceAll(raw, nil), &cfg) != nil {
		return ""
	}
	return cfg.Last.Login
}

// ghUserRE matches the host-level `user: <name>` line (the active account) in gh's hosts.yml.
var ghUserRE = regexp.MustCompile(`(?m)^github\.com:\n(?:[ \t]+.*\n)*?[ \t]{1,4}user:[ \t]*(\S+)`)

// ghConfigDir is gh's config directory: $GH_CONFIG_DIR, else ~/.config/gh.
func ghConfigDir() string {
	if dir := os.Getenv("GH_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gh")
}

// ghHostsFile is gh's hosts.yml path.
func ghHostsFile() string { return filepath.Join(ghConfigDir(), "hosts.yml") }

// GHActiveAccount is gh's active github.com account from hosts.yml (no network), or "" when gh is not logged in.
func GHActiveAccount() string {
	account, _ := ghActiveAccountToken()
	return account
}

type ghActiveTokenKey struct {
	env     [5]string
	path    string
	size    int64
	modTime time.Time
}

var ghActiveTokenCache struct {
	sync.Mutex
	key            ghActiveTokenKey
	account, token string
	failedAt       time.Time // when `gh auth token` last failed under key; skips a retry for authMemoNegativeTTL
}

// authMemoNegativeTTL is how long a failed gh lookup is remembered, so a later `gh auth login` is still seen.
const authMemoNegativeTTL = 30 * time.Second

func ghActiveAccountToken() (account, token string) {
	path := ghHostsFile()
	info, err := os.Stat(path)
	if err != nil {
		return "", ""
	}
	key := ghActiveTokenKey{
		env: [5]string{os.Getenv("GH_CONFIG_DIR"), os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN"),
			os.Getenv("GH_HOST"), os.Getenv("PATH")},
		path: path, size: info.Size(), modTime: info.ModTime(),
	}
	return ghLookupCached(key)
}

// ghLookupCached serves the memoized account/token for key, else reads hosts.yml, runs gh and memoizes.
func ghLookupCached(key ghActiveTokenKey) (account, token string) {
	path := key.path
	ghActiveTokenCache.Lock()
	defer ghActiveTokenCache.Unlock()
	if ghActiveTokenCache.key == key && ghActiveTokenCache.account != "" {
		return ghActiveTokenCache.account, ghActiveTokenCache.token
	}
	if ghActiveTokenCache.key == key && time.Since(ghActiveTokenCache.failedAt) < authMemoNegativeTTL {
		return "", ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	m := ghUserRE.FindSubmatch(raw)
	if m == nil {
		return "", ""
	}
	out, ok := ghAuthToken(key)
	if !ok {
		return "", "" // hosts.yml names a user but gh has no usable token for it
	}
	// Overrides can make the default token differ from the named account's token; retain the original lookup.
	account = string(m[1])
	if !ghEnvOverridesToken() {
		token = strings.TrimSpace(string(out))
	}
	ghActiveTokenCache.key = key
	ghActiveTokenCache.account, ghActiveTokenCache.token = account, token
	ghActiveTokenCache.failedAt = time.Time{}
	return account, token
}

// ghEnvOverridesToken reports whether GH_TOKEN, GITHUB_TOKEN or a non-default GH_HOST can change gh's default token.
func ghEnvOverridesToken() bool {
	host := os.Getenv("GH_HOST")
	return os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" || (host != "" && host != "github.com")
}

// ghAuthToken runs plain `gh auth token` under ghActiveTokenCache's lock; a failure, not a timeout, is remembered.
func ghAuthToken(key ghActiveTokenKey) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err == nil {
		return out, true
	}
	if ctx.Err() == nil {
		ghActiveTokenCache.key, ghActiveTokenCache.account, ghActiveTokenCache.token = key, "", ""
		ghActiveTokenCache.failedAt = time.Now()
	}
	return nil, false
}

type authMemoEntry struct {
	val string
	at  time.Time
}

// authMemo remembers lookups for the process; an empty (failed) result is retried after authMemoNegativeTTL.
type authMemo struct {
	sync.Mutex
	m map[string]authMemoEntry
}

// do returns key's remembered value or runs fn; the lock is held across fn so parallel callers look up once.
func (c *authMemo) do(key string, fn func() string) string {
	c.Lock()
	defer c.Unlock()
	if e, ok := c.m[key]; ok && (e.val != "" || time.Since(e.at) < authMemoNegativeTTL) {
		return e.val
	}
	if c.m == nil {
		c.m = map[string]authMemoEntry{}
	}
	v := fn()
	c.m[key] = authMemoEntry{val: v, at: time.Now()}
	return v
}

var (
	gitHubLoginMemo authMemo // keyed by the token's sha256, never the token
	ghTokenMemo     authMemo // keyed by account plus gh's environment and hosts.yml stamp
)

// GitHubLogin asks api.github.com/user who token belongs to (memoized per token); "" on any failure.
func GitHubLogin(token string) string {
	sum := sha256.Sum256([]byte(token))
	return gitHubLoginMemo.do(hex.EncodeToString(sum[:]), func() string { return fetchGitHubLogin(token) })
}

func fetchGitHubLogin(token string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var u struct {
		Login string `json:"login"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&u) != nil {
		return ""
	}
	return u.Login
}

// ghUsersRE captures the account names under a host's `users:` map in gh's hosts.yml.
var ghUsersRE = regexp.MustCompile(`(?m)^[ \t]{8}([A-Za-z0-9_-]+):`)

// ghHostBlockRE captures the github.com host block (its indented and blank lines), so other hosts' users stay out.
var ghHostBlockRE = regexp.MustCompile(`(?m)^github\.com:[ \t]*\r?\n((?:[ \t]+.*(?:\n|$)|\r?\n)*)`)

// GHAccounts lists every github.com account gh is signed in to (hosts.yml, no network).
func GHAccounts() []string {
	raw, err := os.ReadFile(ghHostsFile())
	if err != nil {
		return nil
	}
	block := ghHostBlockRE.FindSubmatch(raw)
	if block == nil {
		return nil
	}
	var out []string
	for _, m := range ghUsersRE.FindAllSubmatch(block[1], -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// GHToken is gh's token for one signed-in account (`gh auth token -u`, memoized per account), or "".
func GHToken(account string) string {
	dir := ghConfigDir()
	stamp := ""
	if info, err := os.Stat(filepath.Join(dir, "hosts.yml")); err == nil {
		stamp = fmt.Sprint(info.Size(), info.ModTime().UnixNano())
	}
	key := strings.Join([]string{account, dir, os.Getenv("GH_HOST"), os.Getenv("PATH"), stamp}, "\x00")
	return ghTokenMemo.do(key, func() string { return fetchGHToken(account) })
}

func fetchGHToken(account string) string {
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "-u", account).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CopilotToken is the token copilot would run with, resolved quietly (no stderr, no audit) for catalog lookups.
func CopilotToken() string {
	a, ok := copilotAuthSource()
	if !ok {
		return ""
	}
	tok, _ := copilotToken(a)
	return tok
}

// copilotToken fetches a gh-sourced account's token; how names the command used (or why it fell back).
func copilotToken(a CopilotAuth) (tok, how string) {
	how, tok = a.Source, a.token
	if a.Source == srcPinned || a.Source == srcActive {
		how = "gh auth token" // gh's active token came from the plain command
		if a.Source != srcActive || tok == "" {
			how, tok = "gh auth token -u "+a.Account, GHToken(a.Account)
		}
		if tok == "" {
			how = srcCopilot + " (" + how + " failed)"
		}
	}
	return tok, how
}

// AuthStatus is copilotAuthSource, offline: a token-only source leaves Account for Plan's lookup.
func (copilotEngine) AuthStatus() providers.AuthState {
	a, ok, err := copilotAuthSourceErr()
	if !ok {
		return providers.AuthState{Err: err}
	}
	return providers.AuthState{Present: true, Account: a.Account, Source: a.Source, Method: copilotMethod(a.Source), Err: err}
}

// copilotMethod: only copilot's own stored login has a sign-in to re-run; gh and pasted tokens are PATs.
func copilotMethod(src string) string {
	switch src {
	case srcCopilot:
		return providers.AuthCLI
	case srcPinned, srcActive, SrcStoredToken:
		return providers.AuthPAT
	}
	return providers.AuthKey
}

// ShortSource names where an AuthState.Source credential lives, for a menu value: "stored token", "gh active".
func ShortSource(src string) string {
	switch src {
	case SrcStoredToken:
		return "stored token"
	case srcPinned:
		return "gh pinned"
	case srcActive:
		return "gh active"
	case srcCopilot:
		return "copilot login"
	}
	return "$" + src
}

// Plan is the Copilot plan of the account copilot signs in as (its own token, 3 s, cached per account).
func (copilotEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	a, ok := copilotAuthSource()
	tok := ""
	if ok {
		tok, _ = copilotToken(a)
	}
	account := cmp.Or(st.Account, a.Account)
	if account == "" && tok != "" {
		account = GitHubLogin(tok)
	}
	raw := CopilotRawPlan(account, tok)
	return providers.Subscription{Raw: raw, Name: providers.CopilotPlanName(raw), Account: account}
}

// copilotTierCmd is the plan lookup seam; tests replace it. A non-empty token asks as that account, not gh's active one.
var copilotTierCmd = func(ctx context.Context, token string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "/copilot_internal/user")
	spec := agent.EnvSpec{Mode: agent.EnvAgent}
	if token != "" {
		spec.Set = []string{"GH_TOKEN=" + token}
	}
	cmd.Env = spec.Environ(ctx)
	return cmd.Output()
}

// copilotPlanMemo holds one lookup per account, so a sign-in or account switch mid-menu re-asks.
var copilotPlanMemo authMemo

// CopilotRawPlan asks GitHub which Copilot plan account holds ("individual_pro"; 3 s cap); "" when offline.
func CopilotRawPlan(account, token string) string {
	return copilotPlanMemo.do(cmp.Or(account, GHActiveAccount()), func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var u struct {
			Plan string `json:"copilot_plan"`
		}
		if out, err := copilotTierCmd(ctx, token); err == nil {
			_ = json.Unmarshal(out, &u)
		}
		return u.Plan
	})
}
