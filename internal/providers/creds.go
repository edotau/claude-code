package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// ErrPassthrough means the provider forwards the client's own auth; there is no credential to mint.
var ErrPassthrough = errors.New("provider uses passthrough auth")

type cached struct {
	value string
	until time.Time
}

var cmdCache sync.Map // provider name → cached

// Credential resolves env → vault → file → command (cached for TTL).
func Credential(ctx context.Context, p *Provider) (string, error) {
	switch p.Auth.Type {
	case AuthNone:
		return "", nil
	case AuthCLI:
		return "", ErrPassthrough
	}
	if sealedAuth(p.Auth.Type) {
		return authCredential(ctx, p, false)
	}
	var vaultErr error
	for _, n := range keyNames(p) {
		v, _, err := EffectiveSecret(n)
		vaultErr = cmp.Or(vaultErr, err)
		if err == nil && v != "" {
			return v, nil
		}
	}
	if p.Auth.File != "" {
		if b, err := os.ReadFile(expandHome(p.Auth.File)); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, nil
			}
		}
	}
	if p.Auth.Command != "" {
		return commandCredential(ctx, p, false)
	}
	if vaultErr != nil {
		return "", fmt.Errorf("%s: no credential (vault: %v)", p.Name, vaultErr)
	}
	return "", fmt.Errorf("%s: no credential (set %s with `claude-code vault set`%s)", p.Name, orDash(p.Auth.Env), hint(p))
}

// keyNames are the vault/env names a key row's credential may sit under: its new name, then auth.env (old/agent name).
func keyNames(p *Provider) []string {
	var out []string
	if n := KeySecretName(p); n != "" {
		out = append(out, n)
	}
	if p.Auth.Env != "" {
		out = append(out, p.Auth.Env)
	}
	return out
}

// Refresh drops a cached command credential and re-runs it; used after an upstream 401.
func Refresh(ctx context.Context, p *Provider) (string, error) {
	if sealedAuth(p.Auth.Type) {
		return authCredential(ctx, p, true)
	}
	if p.Auth.Command == "" {
		return Credential(ctx, p)
	}
	return commandCredential(ctx, p, true)
}

// flight is one in-progress command mint for a provider; followers wait on done and share val/err.
type flight struct {
	done  chan struct{}
	force bool
	val   string
	err   error
}

var (
	flightMu sync.Mutex
	flights  = map[string]*flight{}
)

func cachedCommand(name string) (string, bool) {
	if c, ok := cmdCache.Load(name); ok && time.Now().Before(c.(cached).until) {
		return c.(cached).value, true
	}
	return "", false
}

// mintOnce is the one mint coordinator: per provider, single-flight; a forced call never joins a non-forced flight.
func mintOnce(ctx context.Context, p *Provider, force bool, mint func(context.Context) (string, error)) (string, error) {
	for {
		flightMu.Lock()
		if !force {
			if v, ok := cachedCommand(p.Name); ok {
				flightMu.Unlock()
				return v, nil
			}
		}
		f, inflight := flights[p.Name]
		if !inflight {
			f = &flight{done: make(chan struct{}), force: force}
			flights[p.Name] = f
			flightMu.Unlock()
			defer func() {
				flightMu.Lock()
				delete(flights, p.Name)
				flightMu.Unlock()
				close(f.done)
			}()
			f.val, f.err = mint(context.WithoutCancel(ctx))
			return f.val, f.err
		}
		flightMu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if f.force || !force {
			return f.val, f.err
		}
	}
}

func commandCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	return mintOnce(ctx, p, force, func(ctx context.Context) (string, error) { return mintCommand(ctx, p, force) })
}

func mintCommand(ctx context.Context, p *Provider, force bool) (string, error) {
	cmd := p.Auth.Command
	if force && p.Auth.RefreshCommand != "" {
		cmd = p.Auth.RefreshCommand
	}
	v, err := runAuthCommand(ctx, p, cmd)
	if err != nil {
		return "", err
	}
	if exp, ok := jwtExp(v); ok && p.Auth.RefreshCommand != "" && cmd != p.Auth.RefreshCommand &&
		time.Until(time.Unix(exp, 0)) < expiryMargin {
		if v, err = runAuthCommand(ctx, p, p.Auth.RefreshCommand); err != nil {
			return "", err
		}
	}
	exp := time.Time{}
	if e, ok := jwtExp(v); ok {
		exp = time.Unix(e, 0)
	}
	if err := finishCredential(p, v, exp); err != nil {
		return "", err
	}
	return v, nil
}

// finishCredential is the shared tail of every mint: issuer check, expiry record, cache until min(ttl, exp-10m).
func finishCredential(p *Provider, v string, exp time.Time) error {
	if err := checkIssuer(p, v); err != nil {
		return err
	}
	recordTokenExpiry(p.Name, v)
	ttl := time.Duration(p.Auth.TTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	until := time.Now().Add(ttl)
	if exp.IsZero() {
		if e, ok := jwtExp(v); ok {
			exp = time.Unix(e, 0)
		}
	}
	if !exp.IsZero() {
		if e := exp.Add(-expiryMargin); e.Before(until) {
			until = e
		}
	}
	cmdCache.Store(p.Name, cached{value: v, until: until})
	return nil
}

// expiryMargin is how close to exp a command token is refreshed, and how early its cache entry ends.
const expiryMargin = 10 * time.Minute

// authCommandTimeout bounds one auth command; a var so tests can shorten it.
var authCommandTimeout = 30 * time.Second

// runAuthCommand runs one auth shell command and returns its trimmed output; errors never carry the value.
func runAuthCommand(ctx context.Context, p *Provider, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, authCommandTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.WaitDelay = 5 * time.Second // orphaned pipeline children must not hold stdout open past the timeout
	out, err := outputInGroup(c)  // the timeout, and the command's end, kill everything it started
	if err != nil {
		return "", authCommandError(ctx, p, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" || v == "null" { // `jq -r .access_token` prints null when the upstream call failed
		return "", fmt.Errorf("%s: auth command printed nothing", p.Name)
	}
	if strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("%s: auth command printed more than one bare token", p.Name)
	}
	return v, nil
}

// networkMarkers in a failed auth command's stderr mean the issuer was unreachable, not that the login expired.
var networkMarkers = []string{"timed out", "timeout", "connection refused", "connection reset", "no such host",
	"could not resolve", "temporary failure", "unavailable", "dns", "proxy", "tls", "certificate"}

// authCommandError classifies a failed auth command (cli|network|auth) and carries its redacted stderr.
func authCommandError(ctx context.Context, p *Provider, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: auth command failed (network: timed out): %w", p.Name, err)
	}
	var ee *exec.ExitError
	detail := ""
	if errors.As(err, &ee) {
		detail = redactDetail(string(ee.Stderr))
	}
	kind := "auth: log in again for this provider"
	switch low := strings.ToLower(detail); {
	case ee != nil && ee.ExitCode() == 127:
		kind = "cli: command not found"
	case slices.ContainsFunc(networkMarkers, func(m string) bool { return strings.Contains(low, m) }):
		kind = "network: check the connection or VPN"
	}
	if detail != "" {
		return fmt.Errorf("%s: auth command failed (%s): %w: %s", p.Name, kind, err, detail)
	}
	return fmt.Errorf("%s: auth command failed (%s): %w", p.Name, kind, err)
}

// redactDetail is stderr's last non-empty line, ≤160 chars, with credential-shaped words masked.
func redactDetail(stderr string) string {
	lines := strings.FieldsFunc(stderr, func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) == 0 {
		return ""
	}
	words := strings.Fields(shapes.Redact(lines[len(lines)-1]))
	for i, w := range words {
		key := strings.ToLower(strings.TrimRight(w, ":="))
		if i+1 < len(words) && slices.Contains([]string{"token", "access_token", "bearer", "authorization"}, key) {
			words[i+1] = "REDACTED"
		}
		if strings.HasPrefix(w, "eyJ") { // a JWT; shapes.Looks covers the prefixed key families
			words[i] = "REDACTED"
		}
	}
	out := strings.Join(words, " ")
	if len(out) > 160 {
		out = out[:160] + "…"
	}
	return out
}

// CredentialPresent checks configured credential sources without decrypting or running commands.
func CredentialPresent(p *Provider) bool {
	switch p.Auth.Type {
	case AuthNone, AuthCLI:
		return true
	}
	if sealedAuth(p.Auth.Type) {
		return HasSecret(AuthSecretName(p)) != ""
	}
	if p.Auth.Command != "" {
		return true
	}
	if slices.ContainsFunc(keyNames(p), func(n string) bool { return HasSecret(n) != "" }) {
		return true
	}
	if p.Auth.File != "" {
		b, err := os.ReadFile(expandHome(p.Auth.File))
		return err == nil && strings.TrimSpace(string(b)) != ""
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func orDash(s string) string {
	if s == "" {
		return "auth.env"
	}
	return s
}

func hint(p *Provider) string {
	if p.Auth.File != "" {
		return ", or " + p.Auth.File
	}
	return ""
}

// ClaudeLoginStatus asks `claude auth status` (JSON) for the interactive session; plan is " (max)"-style or "".
func ClaudeLoginStatus() (loggedIn bool, plan string) {
	loggedIn, plan, _ = ClaudeLoginProbe()
	return loggedIn, plan
}

// ClaudeLoginProbe is ClaudeLoginStatus with the probe error: err != nil means unknown, not logged out.
func ClaudeLoginProbe() (loggedIn bool, plan string, err error) {
	bin, err := paths.VendorBin("claude")
	if err != nil {
		return false, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, bin, "auth", "status").Output()
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil { // logged-out may exit nonzero but still print JSON
		if runErr != nil {
			return false, "", runErr
		}
		return false, "", jerr
	}
	if st.SubscriptionType != "" {
		plan = " (" + st.SubscriptionType + ")"
	}
	return st.LoggedIn, plan, nil
}

// CredentialNames are the env/vault names p's static credential may be read from, sealed (PAT/M2M) names included.
func CredentialNames(p *Provider) []string {
	out := keyNames(p)
	if n := AuthSecretName(p); n != "" {
		out = append(out, n)
	}
	return out
}
