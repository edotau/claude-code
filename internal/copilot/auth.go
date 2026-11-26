package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/github"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/usage"
)

// copilotLaunchTokenFor is the token a launch injects for login ("" = the pin), or "" for copilot's own; logs it.
func copilotLaunchTokenFor(login string) (string, error) {
	a, ok, err := copilotauth.LaunchAuth(login)
	if err != nil || !ok {
		return "", err
	}
	tok, how := copilotauth.ResolveToken(a)
	if a.Source == copilotauth.SrcPinned && tok == "" {
		return "", fmt.Errorf("copilot: no gh token for pinned GitHub user %s (`gh auth token -u` failed "+
			"or timed out); when it is signed out, run `gh auth login`", a.Account)
	}
	fmt.Fprintf(os.Stderr, "copilot: GitHub account %s (%s)\n", cmp.Or(a.Account, "from token"), how)
	audit.Log(audit.CopilotAuth, cmp.Or(a.Account, string(a.Source)), how)
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
	pin, err := copilotauth.PinLoginErr()
	if err != nil {
		return "cannot read the provider registry to check the pinned GitHub account: " + err.Error(), false
	}
	if pin == "" {
		return ownLoginHeadroom(ctx)
	}
	return copilotHeadroom(ctx, pin)
}

// ownLoginHeadroom gates copilot's stored login unpinned: its credits when gh holds it, else workable unverified.
func ownLoginHeadroom(ctx context.Context) (detail string, ok bool) {
	a, found := copilotauth.AuthSource()
	switch {
	case !found || a.Source != copilotauth.SrcCopilot:
		return copilotHeadroom(ctx, "")
	case slices.Contains(github.GHAccounts(), a.Account) && creditsKeyedTo(a.Account):
		return copilotHeadroom(ctx, a.Account)
	}
	// tried first per the owner's order; an auth or quota error fails over like any hop
	return a.Account + " unverified (copilot's own login; no gh credit reading for it)", true
}

// creditsKeyedTo: login's route-account key reads login's own credits, not a profile named login with another Login.
func creditsKeyedTo(login string) bool {
	reg, _ := providers.Load()
	return providers.GithubAccountLogin(reg, providers.GithubAccountName(reg, login)) == login
}

// AuthStatus is copilotauth.AuthSource, offline: a token-only source leaves Account for Plan's lookup.
func (copilotEngine) AuthStatus() providers.AuthState {
	a, ok, err := copilotauth.AuthSourceErr()
	if !ok {
		return providers.AuthState{Err: err}
	}
	return providers.AuthState{Present: true, Account: a.Account, Source: string(a.Source), Method: copilotMethod(a.Source), Err: err}
}

func (copilotEngine) Presence(_ context.Context, cell router.Cell) error {
	if cell.Account != "" && !strings.HasPrefix(cell.Account, "github·") {
		return nil
	}
	u, err := copilotauth.PinLoginErr()
	if err != nil {
		return fmt.Errorf("cannot read the provider registry to check the pinned gh user: %w", err)
	}
	if u != "" && !slices.Contains(github.GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	return nil
}

// copilotMethod: only copilot's own stored login has a sign-in to re-run; gh and pasted tokens are PATs.
func copilotMethod(src copilotauth.Source) string {
	switch src {
	case copilotauth.SrcCopilot:
		return providers.AuthCLI
	case copilotauth.SrcPinned, copilotauth.SrcActive, copilotauth.SrcStoredToken:
		return providers.AuthPAT
	}
	return providers.AuthKey
}

// Plan is the Copilot plan of the account copilot signs in as (its own token, 3 s, cached per account).
func (copilotEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	a, ok := copilotauth.AuthSource()
	tok := ""
	if ok {
		tok, _ = copilotauth.ResolveToken(a)
	}
	account := cmp.Or(st.Account, a.Account)
	if account == "" && tok != "" {
		account = github.GitHubLogin(tok)
	}
	// a tokenless lookup asks as gh's active account, so copilot's own login asks with its gh token or not at all
	if a.Source == copilotauth.SrcCopilot && tok == "" {
		if tok = github.GHToken(account); tok == "" {
			return providers.Subscription{Account: account}
		}
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

var copilotPlanMemo = paths.NewMemo[string, string]("copilot.plan")

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
