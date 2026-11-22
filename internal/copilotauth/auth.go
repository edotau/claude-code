package copilotauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/github"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// TokenEnvs is copilot's documented env precedence (`copilot help environment`).
var TokenEnvs = []string{providers.CopilotTokenEnv, "GH_TOKEN", "GITHUB_TOKEN"}

// Auth is the GitHub identity copilot authenticates as and where its credential comes from.
type Auth struct {
	// GitHub login; "" when it needs a network lookup the caller skipped, or that failed
	Account string
	// SrcPinned, SrcActive, an env var name, or SrcCopilot
	Source string
	Token  string
}

// Credential sources, highest first; the gh ones resolve a token at launch with `gh auth token [-u]`.
const (
	SrcPinned  = "gh (pinned)"
	SrcActive  = "gh (active)"
	SrcCopilot = "copilot login"
	// SrcStoredToken is a COPILOT_GITHUB_TOKEN pasted via configure (sealed in the vault).
	SrcStoredToken = providers.CopilotTokenEnv + " (stored)"
)

// PinLoginErr is the pinned copilot profile's GitHub login ("" = unpinned) and the providers.Load error.
func PinLoginErr() (string, error) {
	reg, err := providers.Load()
	login, _ := providers.CopilotLogin(reg)
	return login, err
}

// AuthSource (offline): pinned gh login, stored token, gh active, env token, copilot login; none if no registry.
func AuthSource() (Auth, bool) {
	a, ok, err := AuthSourceErr()
	// an unreadable registry may hide a pin, so no other account is used
	return a, ok && err == nil
}

// AuthSourceErr is AuthSource plus the registry load error; for display, a load error falls through.
func AuthSourceErr() (Auth, bool, error) {
	pin, loadErr := PinLoginErr()
	a, ok := AuthSourceFor(pin)
	return a, ok, loadErr
}

// AuthSourceFor is the credential source for pin ("" = unpinned).
func AuthSourceFor(pin string) (Auth, bool) {
	// a pinned profile with no login falls through to gh's active account
	if pin != "" {
		if slices.Contains(github.GHAccounts(), pin) {
			return Auth{Account: pin, Source: SrcPinned}, true
		}
		// a pinned account that can't be used never falls back to another one
		return Auth{}, false
	}
	if tok, src, ok := vault.Get(providers.CopilotTokenEnv); ok && src != "env" {
		return Auth{Source: SrcStoredToken, Token: strings.TrimSpace(tok)}, true
	}
	if login, tok := github.ActiveAccountToken(); login != "" {
		return Auth{Account: login, Source: SrcActive, Token: tok}, true
	}
	for _, k := range TokenEnvs {
		if tok := strings.TrimSpace(os.Getenv(k)); tok != "" {
			return Auth{Source: k, Token: tok}, true
		}
	}
	if login := StoredLogin(); login != "" {
		return Auth{Account: login, Source: SrcCopilot}, true
	}
	return Auth{}, false
}

// LaunchAuth is the account a launch runs on: login's gh account, else the pin's; ok false = copilot's own credential.
func LaunchAuth(login string) (Auth, bool, error) {
	// an explicit --profile <github row> runs on that row's gh login, never the record's or a stored token
	if login != "" {
		a, ok := AuthSourceFor(login)
		if !ok {
			return a, false, fmt.Errorf("copilot: GitHub user %s is not logged in to gh; run `gh auth login`", login)
		}
		return a, true, nil
	}
	pin, err := PinLoginErr()
	if err != nil {
		return Auth{}, false, fmt.Errorf("copilot: cannot read the provider registry to check the pinned GitHub "+
			"account (launch refused): %w", err)
	}
	a, ok := AuthSourceFor(pin)
	// the harness fails rather than run on another account
	if !ok && pin != "" {
		return a, false, fmt.Errorf("copilot: pinned GitHub user %s is not logged in to gh; run `claude-code configure "+
			"github` (no other account is used)", pin)
	}
	return a, ok, nil
}

// commentRE strips the // comment lines copilot writes at the top of config.json.
var commentRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*//.*$`) })

// StripComments removes the // comment lines copilot writes at the top of config.json.
func StripComments(raw []byte) []byte { return commentRE().ReplaceAll(raw, nil) }

// StoredLogin is lastLoggedInUser.login from copilot's config ($COPILOT_HOME or ~/.copilot), or "".
func StoredLogin() string {
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
	if json.Unmarshal(StripComments(raw), &cfg) != nil {
		return ""
	}
	return cfg.Last.Login
}

// ResolveToken fetches a gh-sourced account's token; how names the command used (or why it fell back).
func ResolveToken(a Auth) (tok, how string) {
	how, tok = a.Source, a.Token
	if a.Source == SrcPinned || a.Source == SrcActive {
		how = "gh auth token"
		if a.Source != SrcActive || tok == "" {
			how, tok = "gh auth token -u "+a.Account, github.GHToken(a.Account)
		}
		if tok == "" {
			how = SrcCopilot + " (" + how + " failed)"
		}
	}
	return tok, how
}

// ShortSource names where an AuthState.Source credential lives, for a menu value: "stored token", "gh active".
func ShortSource(src string) string {
	switch src {
	case SrcStoredToken:
		return "stored token"
	case SrcPinned:
		return "gh pinned"
	case SrcActive:
		return "gh active"
	case SrcCopilot:
		return "copilot login"
	}
	return "$" + src
}
