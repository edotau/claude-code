package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/usage"
	"github.com/edotau/claude-code/internal/vault"
)

// copilotTokenEnvs is copilot's documented env precedence (`copilot help environment`).
var copilotTokenEnvs = []string{providers.CopilotTokenEnv, "GH_TOKEN", "GITHUB_TOKEN"}

// copilotAuth is the GitHub identity copilot authenticates as and where its credential comes from.
type copilotAuth struct {
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

// copilotPinLoginErr is the pinned copilot profile's GitHub login ("" = unpinned) and the providers.Load error.
func copilotPinLoginErr() (string, error) {
	reg, err := providers.Load()
	login, _ := providers.CopilotLogin(reg)
	return login, err
}

// copilotAuthSource (no network): the pinned profile's gh login, a stored token, gh's active account, then
// copilot's own order (env token, its stored login) when gh holds no account; none when the registry can't load.
func copilotAuthSource() (copilotAuth, bool) {
	a, ok, err := copilotAuthSourceErr()
	return a, ok && err == nil // an unreadable registry may hide a pin, so no other account is used
}

// copilotAuthSourceErr is copilotAuthSource plus the registry load error; for display, a load error falls through.
func copilotAuthSourceErr() (copilotAuth, bool, error) {
	pin, loadErr := copilotPinLoginErr()
	a, ok := authSourceFor(pin)
	return a, ok, loadErr
}

// authSourceFor resolves the source for the pinned GitHub login pin ("" = unpinned).
func authSourceFor(pin string) (copilotAuth, bool) {
	if pin != "" { // a pinned profile with no login falls through to gh's active account
		if slices.Contains(GHAccounts(), pin) {
			return copilotAuth{Account: pin, Source: srcPinned}, true
		}
		return copilotAuth{}, false // a pinned account that can't be used never falls back to another one
	}
	if tok, src, ok := vault.Get(providers.CopilotTokenEnv); ok && src != "env" {
		return copilotAuth{Source: SrcStoredToken, token: strings.TrimSpace(tok)}, true
	}
	if login, tok := ghActiveAccountToken(); login != "" {
		return copilotAuth{Account: login, Source: srcActive, token: tok}, true
	}
	for _, k := range copilotTokenEnvs {
		if tok := strings.TrimSpace(os.Getenv(k)); tok != "" {
			return copilotAuth{Source: k, token: tok}, true
		}
	}
	if login := copilotStoredLogin(); login != "" {
		return copilotAuth{Account: login, Source: srcCopilot}, true
	}
	return copilotAuth{}, false
}

// copilotLaunchTokenFor is the token a launch injects (COPILOT_GITHUB_TOKEN for the CLI, GitHubToken for the SDK),
// or "" to leave copilot on its own credential; logs the source. login is the target GitHub row's; "" uses the pin.
func copilotLaunchTokenFor(login string) (string, error) {
	var a copilotAuth
	var ok bool
	if login != "" { // an explicit --profile <github row> runs on that row's gh login, never the record's or a stored token
		a, ok = authSourceFor(login)
		if !ok {
			return "", fmt.Errorf("copilot: GitHub user %s is not logged in to gh; run `gh auth login`", login)
		}
	} else {
		pin, err := copilotPinLoginErr()
		if err != nil {
			return "", fmt.Errorf("copilot: cannot read the provider registry to check the pinned GitHub account "+
				"(launch refused): %w", err)
		}
		if a, ok = authSourceFor(pin); !ok && pin != "" { // the harness fails rather than run on another account
			return "", fmt.Errorf("copilot: pinned GitHub user %s is not logged in to gh; run `claude-code configure "+
				"github` (no other account is used)", pin)
		}
	}
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

// RefreshAccount re-fetches a stale account for copilotUsedPct; a var so tests stay offline.
var RefreshAccount = usage.RefreshAccount

// CopilotUsedPct is a var so a test can pin a login's credit share.
var CopilotUsedPct = copilotUsedPct

// copilotUsedPct is github·<profile>'s used share from route state, refreshed through RefreshAccount past its TTL.
func copilotUsedPct(ctx context.Context, login string) (float64, bool) {
	key, now := copilotStateKey(login), time.Now()
	e := router.LoadState()[key]
	if e.ExhaustedUntil.After(now) {
		return router.ExhaustedPct, true
	}
	if e.Stale(now) {
		ctx, cancel := context.WithTimeout(ctx, refreshCap)
		defer cancel()
		if !RefreshAccount(ctx, key) {
			return 0, false
		}
		e = router.LoadState()[key]
	}
	if now.Sub(e.Seen) > router.StaleAfter {
		return 0, false // a recent check found no reading: unverified, as when that refresh failed
	}
	return e.UsedPct, true
}

// refreshCap bounds the launch-time refresh; past it the launch runs unverified rather than wait.
const refreshCap = time.Second

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
		return unverifiedDetail(user), false
	}
	return fmt.Sprintf("%s %.1f%% of credits used", user, pct), pct < copilotWorkablePct
}

// copilotStateKey is login's route-state key, github·<profile>.
func copilotStateKey(login string) string {
	reg, _ := providers.Load()
	return "github·" + providers.GithubAccountName(reg, login)
}

// unverifiedDetail names why user has no reading: a recent check's note, else plain "unverified".
func unverifiedDetail(user string) string {
	e := router.LoadState()[copilotStateKey(user)]
	switch {
	case e.Stale(time.Now()) || e.CheckNote == "":
		return user + " unverified"
	case strings.HasPrefix(e.CheckNote, usage.NoPremiumCredits):
		return user + " has " + e.CheckNote
	}
	return user + " unverified (" + e.CheckNote + ")"
}

// pinnedHeadroom is copilotHeadroom on login, else on the pinned login; an unreadable registry is unworkable.
func pinnedHeadroom(ctx context.Context, login string) (detail string, ok bool) {
	if login != "" {
		return copilotHeadroom(ctx, login)
	}
	pin, err := copilotPinLoginErr()
	if err != nil {
		return "cannot read the provider registry to check the pinned GitHub account: " + err.Error(), false
	}
	return copilotHeadroom(ctx, pin)
}

// commentRE strips the // comment lines copilot writes at the top of config.json.
var commentRE = regexp.MustCompile(`(?m)^[ \t]*//.*$`)

// copilotStoredLogin is lastLoggedInUser.login from copilot's config ($COPILOT_HOME or ~/.copilot), or "".
func copilotStoredLogin() string {
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

// SwitchGH makes login gh's active account (`gh auth switch`); a seam so tests never touch the real gh.
var SwitchGH = func(login string) error {
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "switch", "--hostname", "github.com", "--user", login).CombinedOutput()
	ghActiveTokenMemo.Drop()
	if err != nil {
		return fmt.Errorf("gh auth switch --user %s: %w: %s", login, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type ghActiveTokenKey struct {
	env     [5]string
	path    string
	size    int64
	modTime time.Time
}

// ghActive is gh's active account and, unless env overrides gh's default, that account's token.
type ghActive struct{ account, token string }

// ghActiveTokenMemo keeps a found account until SwitchGH, a `gh auth token` failure for authMemoNegativeTTL.
var ghActiveTokenMemo paths.Memo[ghActiveTokenKey, ghActive]

// errGHTokenFailed is a `gh auth token` failure that is not a timeout, so it is worth remembering.
var errGHTokenFailed = errors.New("gh auth token failed")

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
	v, _ := ghActiveTokenMemo.Do(key, keepGHActive, func() (ghActive, error) { return ghLookup(path) })
	return v.account, v.token
}

// keepGHActive keeps a found account until Drop, a gh failure for authMemoNegativeTTL, anything else not at all.
func keepGHActive(v ghActive, err error) time.Duration {
	switch {
	case v.account != "":
		return 0
	case errors.Is(err, errGHTokenFailed):
		return authMemoNegativeTTL
	}
	return -1
}

// ghLookup reads hosts.yml's active user and runs gh for its token.
func ghLookup(path string) (ghActive, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ghActive{}, nil
	}
	m := ghUserRE.FindSubmatch(raw)
	if m == nil {
		return ghActive{}, nil
	}
	out, err := ghAuthToken()
	if err != nil {
		return ghActive{}, err // hosts.yml names a user but gh has no usable token for it
	}
	// Overrides can make the default token differ from the named account's token; retain the original lookup.
	a := ghActive{account: string(m[1])}
	if !ghEnvOverridesToken() {
		a.token = strings.TrimSpace(string(out))
	}
	return a, nil
}

// ghEnvOverridesToken reports whether GH_TOKEN, GITHUB_TOKEN or a non-default GH_HOST can change gh's default token.
func ghEnvOverridesToken() bool {
	host := os.Getenv("GH_HOST")
	return os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" || (host != "" && host != "github.com")
}

// ghAuthToken runs `gh auth token` plus extra; a failure, not a timeout, is errGHTokenFailed.
func ghAuthToken(extra ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", append([]string{"auth", "token"}, extra...)...).Output()
	if err != nil && ctx.Err() == nil {
		return nil, errGHTokenFailed
	}
	return out, err
}

// keepAuth keeps a found value until Drop and an empty (failed) one for authMemoNegativeTTL.
func keepAuth(v string, _ error) time.Duration {
	if v != "" {
		return 0
	}
	return authMemoNegativeTTL
}

var (
	gitHubLoginMemo paths.Memo[string, string] // keyed by the token's sha256, never the token
	ghTokenMemo     paths.Memo[string, string] // keyed by account plus gh's environment and hosts.yml stamp
)

// GitHubLogin asks api.github.com/user who token belongs to (memoized per token); "" on any failure.
func GitHubLogin(token string) string {
	key := textutil.SHA256Hex([]byte(token))
	v, _ := gitHubLoginMemo.Do(key, keepAuth, func() (string, error) { return fetchGitHubLogin(token), nil })
	return v
}

// gitHubUserURL is GitHub's who-am-I endpoint; a var so tests aim it at an httptest server.
var gitHubUserURL = "https://api.github.com/user"

// maxGitHubUserBody caps the /user read; a longer body truncates, fails to decode and reads as no login.
const maxGitHubUserBody = 64 << 10

func fetchGitHubLogin(token string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubUserURL, nil)
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
	body := io.LimitReader(resp.Body, maxGitHubUserBody)
	if resp.StatusCode != http.StatusOK || json.NewDecoder(body).Decode(&u) != nil {
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
	v, _ := ghTokenMemo.Do(key, keepAuth, func() (string, error) {
		out, err := ghAuthToken("-u", account)
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	})
	return v
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
func copilotToken(a copilotAuth) (tok, how string) {
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
	raw := copilotRawPlan(account, tok)
	return providers.Subscription{Raw: raw, Name: providers.CopilotPlanName(raw), Account: account}
}

// copilotTierCmd is the plan lookup seam; tests replace it. A non-empty token asks as that account, not gh's active one.
var copilotTierCmd = func(ctx context.Context, token string) ([]byte, error) {
	spec := agent.EnvSpec{Mode: agent.EnvAgent}
	if token != "" {
		spec.Set = []string{"GH_TOKEN=" + token}
	}
	return agent.ProbeCLI(agent.ProbeContext(ctx, "copilot"), "gh", spec, false, "api", "/copilot_internal/user")
}

// copilotPlanMemo holds one lookup per account, so a sign-in or account switch mid-menu re-asks.
var copilotPlanMemo paths.Memo[string, string]

// copilotRawPlan asks GitHub which Copilot plan account holds ("individual_pro"; 3 s cap); "" when offline.
func copilotRawPlan(account, token string) string {
	v, _ := copilotPlanMemo.Do(cmp.Or(account, GHActiveAccount()), keepAuth, func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var u struct {
			Plan string `json:"copilot_plan"`
		}
		if out, err := copilotTierCmd(ctx, token); err == nil {
			_ = json.Unmarshal(out, &u)
		}
		return u.Plan, nil
	})
	return v
}
