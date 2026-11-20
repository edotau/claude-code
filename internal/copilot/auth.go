package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/github"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/usage"
	"github.com/edotau/claude-code/internal/vault"
)

// copilotTokenEnvs is copilot's documented env precedence (`copilot help environment`).
var copilotTokenEnvs = []string{providers.CopilotTokenEnv, "GH_TOKEN", "GITHUB_TOKEN"}

// copilotAuth is the GitHub identity copilot authenticates as and where its credential comes from.
type copilotAuth struct {
	// GitHub login; "" when it needs a network lookup the caller skipped, or that failed
	Account string
	// srcPinned, srcActive, an env var name, or srcCopilot
	Source string
	token  string
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

// copilotAuthSource (offline): pinned gh login, stored token, gh active, env token, copilot login; none if no registry.
func copilotAuthSource() (copilotAuth, bool) {
	a, ok, err := copilotAuthSourceErr()
	// an unreadable registry may hide a pin, so no other account is used
	return a, ok && err == nil
}

// copilotAuthSourceErr is copilotAuthSource plus the registry load error; for display, a load error falls through.
func copilotAuthSourceErr() (copilotAuth, bool, error) {
	pin, loadErr := copilotPinLoginErr()
	a, ok := authSourceFor(pin)
	return a, ok, loadErr
}

func authSourceFor(pin string) (copilotAuth, bool) {
	// a pinned profile with no login falls through to gh's active account
	if pin != "" {
		if slices.Contains(github.GHAccounts(), pin) {
			return copilotAuth{Account: pin, Source: srcPinned}, true
		}
		// a pinned account that can't be used never falls back to another one
		return copilotAuth{}, false
	}
	if tok, src, ok := vault.Get(providers.CopilotTokenEnv); ok && src != "env" {
		return copilotAuth{Source: SrcStoredToken, token: strings.TrimSpace(tok)}, true
	}
	if login, tok := github.ActiveAccountToken(); login != "" {
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

// copilotLaunchTokenFor is the token a launch injects for login ("" = the pin), or "" for copilot's own; logs it.
func copilotLaunchTokenFor(login string) (string, error) {
	a, ok, err := launchAuth(login)
	if err != nil || !ok {
		return "", err
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

// launchAuth is the account a launch runs on: login's gh account, else the pin's; ok false = copilot's own credential.
func launchAuth(login string) (copilotAuth, bool, error) {
	// an explicit --profile <github row> runs on that row's gh login, never the record's or a stored token
	if login != "" {
		a, ok := authSourceFor(login)
		if !ok {
			return a, false, fmt.Errorf("copilot: GitHub user %s is not logged in to gh; run `gh auth login`", login)
		}
		return a, true, nil
	}
	pin, err := copilotPinLoginErr()
	if err != nil {
		return copilotAuth{}, false, fmt.Errorf("copilot: cannot read the provider registry to check the pinned GitHub "+
			"account (launch refused): %w", err)
	}
	a, ok := authSourceFor(pin)
	// the harness fails rather than run on another account
	if !ok && pin != "" {
		return a, false, fmt.Errorf("copilot: pinned GitHub user %s is not logged in to gh; run `claude-code configure "+
			"github` (no other account is used)", pin)
	}
	return a, ok, nil
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
		// a recent check found no reading: unverified, as when that refresh failed
		return 0, false
	}
	return e.UsedPct, true
}

// refreshCap bounds the launch-time refresh; past it the launch runs unverified rather than wait.
const refreshCap = time.Second

// copilotWorkablePct is the used credit share at which a copilot GitHub-login dispatch is no longer started.
const copilotWorkablePct = 95.0

// copilotHeadroom: user is a gh login verified under copilotWorkablePct; detail names the account and its share.
func copilotHeadroom(ctx context.Context, user string) (detail string, ok bool) {
	switch {
	case user == "":
		return "no pinned GitHub account", false
	case !slices.Contains(github.GHAccounts(), user):
		return user + " not logged in to gh", false
	}
	pct, ok := CopilotUsedPct(ctx, user)
	if !ok {
		return unverifiedDetail(user), false
	}
	return fmt.Sprintf("%s %.1f%% of credits used", user, pct), pct < copilotWorkablePct
}

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
var commentRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*//.*$`) })

// copilotStoredLogin is lastLoggedInUser.login from copilot's config ($COPILOT_HOME or ~/.copilot), or "".
func copilotStoredLogin() string {
	dir, err := paths.CopilotHome()
	if err != nil {
		return ""
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
	if json.Unmarshal(commentRE().ReplaceAll(raw, nil), &cfg) != nil {
		return ""
	}
	return cfg.Last.Login
}

// copilotToken fetches a gh-sourced account's token; how names the command used (or why it fell back).
func copilotToken(a copilotAuth) (tok, how string) {
	how, tok = a.Source, a.token
	if a.Source == srcPinned || a.Source == srcActive {
		how = "gh auth token"
		if a.Source != srcActive || tok == "" {
			how, tok = "gh auth token -u "+a.Account, github.GHToken(a.Account)
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
		account = github.GitHubLogin(tok)
	}
	raw := copilotRawPlan(account, tok)
	return providers.Subscription{Raw: raw, Name: providers.CopilotPlanName(raw), Account: account}
}

// copilotTierCmd is the plan lookup seam (tests replace it); a non-empty token asks as that account, not gh's active.
var copilotTierCmd = func(ctx context.Context, token string) ([]byte, error) {
	spec := agent.EnvSpec{Mode: agent.EnvAgent}
	if token != "" {
		spec.Set = []string{"GH_TOKEN=" + token}
	}
	return agent.ProbeCLI(agent.ProbeContext(ctx, "copilot"), "gh", spec, false, "api", "/copilot_internal/user")
}

var copilotPlanMemo paths.Memo[string, string]

// copilotRawPlan asks GitHub which Copilot plan account holds ("individual_pro"; 3 s cap); "" when offline.
func copilotRawPlan(account, token string) string {
	v, _ := copilotPlanMemo.Do(cmp.Or(account, github.GHActiveAccount()), github.KeepAuth, func() (string, error) {
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
