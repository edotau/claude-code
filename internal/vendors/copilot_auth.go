package vendors

import (
	"cmp"
	"context"
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

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

// CopilotTokenEnv is copilot's highest-precedence token var; the harness sets it at launch from `gh auth token`.
const CopilotTokenEnv = "COPILOT_GITHUB_TOKEN"

// copilotTokenEnvs is copilot's documented env precedence (`copilot help environment`).
var copilotTokenEnvs = []string{CopilotTokenEnv, "GH_TOKEN", "GITHUB_TOKEN"}

// copilotTokenPrefixes are GitHub's own token prefixes (gho_/ghp_/ghu_) plus the newer fine-grained github_pat_.
var copilotTokenPrefixes = []string{"gho_", "ghp_", "ghu_", "github_pat_"}

// ValidCopilotToken reports whether tok has the shape of a real GitHub token, rejecting an obvious
// typo or truncated paste before it is sealed under CopilotTokenEnv.
func ValidCopilotToken(tok string) bool {
	if len(tok) < 20 {
		return false
	}
	for _, p := range copilotTokenPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return false
}

// CopilotAuth is the GitHub identity copilot authenticates as and where its credential comes from.
type CopilotAuth struct {
	Account string // GitHub login; "" when it needs a network lookup the caller skipped, or that failed
	Source  string // srcPinned, srcActive, an env var name, or srcCopilot
	token   string
}

// Credential sources, highest first; the gh ones resolve a token at launch with `gh auth token --user`.
const (
	srcPinned  = "gh (pinned)"
	srcActive  = "gh (active)"
	srcCopilot = "copilot login"
	// SrcStoredToken is a COPILOT_GITHUB_TOKEN pasted via configure (sealed in the vault).
	SrcStoredToken = CopilotTokenEnv + " (stored)"
)

// copilotAuthSource (no network): the pinned gh user (COPILOT_GITHUB_USER), a stored token, gh's active account, then
// copilot's own order (env token, its stored login) when gh holds no account.
func copilotAuthSource() (CopilotAuth, bool) {
	if pin := providers.Pin(providers.PinGitHubUser); pin != "" {
		if slices.Contains(GHAccounts(), pin) {
			return CopilotAuth{Account: pin, Source: srcPinned}, true
		}
		return CopilotAuth{}, false // a pinned account that can't be used never falls back to another one
	}
	if tok, src, ok := secrets.Get(CopilotTokenEnv); ok && src != "env" {
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
	if err := CopilotPinError(); err != nil {
		return "", err
	}
	a, ok := copilotAuthSource()
	if !ok {
		return "", nil
	}
	tok, how := copilotToken(a)
	if a.Source == srcPinned && tok == "" {
		return "", fmt.Errorf("copilot: no gh token for pinned GitHub user %s; run `claude-code configure github`", a.Account)
	}
	fmt.Fprintf(os.Stderr, "copilot: GitHub account %s (%s)\n", cmp.Or(a.Account, "from token"), how)
	audit.Log(audit.CopilotAuth, cmp.Or(a.Account, a.Source), how)
	return tok, nil
}

// CopilotPinError is non-nil when COPILOT_GITHUB_USER is pinned but that user is not a gh login: the harness
// fails rather than run copilot on a stored token or another account.
func CopilotPinError() error {
	pin := providers.Pin(providers.PinGitHubUser)
	if pin == "" || slices.Contains(GHAccounts(), pin) {
		return nil
	}
	return fmt.Errorf("copilot: pinned GitHub user %s is not logged in to gh; run `claude-code configure github` "+
		"(no other account is used)", pin)
}

// ResolveCopilotAuth is copilotAuthSource plus a GitHub /user lookup (3 s) naming a token's account.
func ResolveCopilotAuth() (CopilotAuth, bool) {
	a, ok := copilotAuthSource()
	if ok && a.Account == "" && a.token != "" {
		a.Account = GitHubLogin(a.token)
	}
	return a, ok
}

// Label renders "account via source", or the source alone when the account is unknown.
func (a CopilotAuth) Label() string {
	if a.Account == "" {
		return a.Source
	}
	return a.Account + " via " + a.Source
}

// commentRE strips the // comment lines copilot writes at the top of config.json.
var commentRE = regexp.MustCompile(`(?m)^[ \t]*//.*$`)

// CopilotStoredLogin is lastLoggedInUser.login from copilot's config ($COPILOT_HOME or ~/.copilot), or "".
func CopilotStoredLogin() string {
	dir := os.Getenv("COPILOT_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".copilot")
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
}

func ghActiveAccountToken() (account, token string) {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "gh")
	}
	path := filepath.Join(dir, "hosts.yml")
	info, err := os.Stat(path)
	if err != nil {
		return "", ""
	}
	key := ghActiveTokenKey{
		env: [5]string{os.Getenv("GH_CONFIG_DIR"), os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN"),
			os.Getenv("GH_HOST"), os.Getenv("PATH")},
		path: path, size: info.Size(), modTime: info.ModTime(),
	}
	ghActiveTokenCache.Lock()
	defer ghActiveTokenCache.Unlock()
	if ghActiveTokenCache.key == key && ghActiveTokenCache.account != "" {
		return ghActiveTokenCache.account, ghActiveTokenCache.token
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	m := ghUserRE.FindSubmatch(raw)
	if m == nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", "" // hosts.yml names a user but gh has no usable token for it
	}
	// Overrides can make the default token differ from the named account's token; retain the original lookup.
	if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" ||
		(os.Getenv("GH_HOST") != "" && os.Getenv("GH_HOST") != "github.com") {
		account = string(m[1])
	} else {
		account, token = string(m[1]), strings.TrimSpace(string(out))
	}
	ghActiveTokenCache.key = key
	ghActiveTokenCache.account, ghActiveTokenCache.token = account, token
	return account, token
}

// GitHubLogin asks api.github.com/user who token belongs to; "" on any failure.
func GitHubLogin(token string) string {
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

// ghUsersRE captures the account names under github.com's `users:` map in gh's hosts.yml.
var ghUsersRE = regexp.MustCompile(`(?m)^[ \t]{8}([A-Za-z0-9_-]+):`)

// GHAccounts lists every github.com account gh is signed in to (hosts.yml, no network).
func GHAccounts() []string {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "gh")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range ghUsersRE.FindAllSubmatch(raw, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// GHToken is gh's token for one signed-in account (`gh auth token -u`), or "".
func GHToken(account string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "-u", account).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// init wires providers.CopilotTokenFn: internal/providers cannot import internal/vendors (vendors already
// imports providers), so providers' own copilot catalog listing gets the launch-account token this way.
func init() {
	providers.CopilotTokenFn = CopilotToken
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
		how = "gh auth token --user " + a.Account
		if a.Source != srcActive || tok == "" {
			tok = GHToken(a.Account)
		}
		if tok == "" {
			how = srcCopilot + " (" + how + " failed)"
		}
	}
	return tok, how
}
