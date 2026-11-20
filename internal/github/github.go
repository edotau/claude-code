// Package github reads gh's signed-in accounts and tokens and resolves a token's GitHub login.
package github

import (
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
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// ghUserRE matches the host-level `user: <name>` line (the active account) in gh's hosts.yml.
var ghUserRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^github\.com:\n(?:[ \t]+.*\n)*?[ \t]{1,4}user:[ \t]*(\S+)`)
})

// ghConfigDir is gh's config directory: $GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh, else ~/.config/gh.
func ghConfigDir() string {
	if dir := os.Getenv("GH_CONFIG_DIR"); dir != "" {
		return dir
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gh")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gh")
}

func ghHostsFile() string { return filepath.Join(ghConfigDir(), "hosts.yml") }

// GHActiveAccount is gh's active github.com account from hosts.yml (no network), or "" when gh is not logged in.
func GHActiveAccount() string {
	account, _ := ActiveAccountToken()
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

// ActiveAccountToken is gh's active account and, unless env overrides gh's default, its token.
func ActiveAccountToken() (account, token string) {
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

func ghLookup(path string) (ghActive, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ghActive{}, nil
	}
	m := ghUserRE().FindSubmatch(raw)
	if m == nil {
		return ghActive{}, nil
	}
	out, err := ghAuthToken()
	if err != nil {
		// hosts.yml names a user but gh has no usable token for it
		return ghActive{}, err
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

// KeepAuth keeps a found value until Drop and an empty one for authMemoNegativeTTL.
func KeepAuth(v string, _ error) time.Duration {
	if v != "" {
		return 0
	}
	return authMemoNegativeTTL
}

var (
	// keyed by the token's sha256, never the token
	gitHubLoginMemo paths.Memo[string, string]
	// keyed by account plus gh's environment and hosts.yml stamp
	ghTokenMemo paths.Memo[string, string]
)

// GitHubLogin asks api.github.com/user who token belongs to (memoized per token); "" on any failure.
func GitHubLogin(token string) string {
	key := textutil.SHA256Hex([]byte(token))
	v, _ := gitHubLoginMemo.Do(key, KeepAuth, func() (string, error) { return fetchGitHubLogin(token), nil })
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

var ghUsersRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]{8}([A-Za-z0-9_-]+):`) })

// ghHostBlockRE captures the github.com host block (its indented and blank lines), so other hosts' users stay out.
var ghHostBlockRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^github\.com:[ \t]*\r?\n((?:[ \t]+.*(?:\n|$)|\r?\n)*)`)
})

// GHAccounts lists the github.com accounts gh is signed in to, offline.
func GHAccounts() []string {
	raw, err := os.ReadFile(ghHostsFile())
	if err != nil {
		return nil
	}
	block := ghHostBlockRE().FindSubmatch(raw)
	if block == nil {
		return nil
	}
	var out []string
	for _, m := range ghUsersRE().FindAllSubmatch(block[1], -1) {
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
	v, _ := ghTokenMemo.Do(key, KeepAuth, func() (string, error) {
		out, err := ghAuthToken("-u", account)
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	})
	return v
}
