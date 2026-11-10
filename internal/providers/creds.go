package providers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrPassthrough means the provider forwards the client's own auth; there is no credential to mint.
var ErrPassthrough = errors.New("provider uses passthrough auth")

type cached struct {
	value string
	until time.Time
}

var cmdCache sync.Map // provider name → cached

// Credential resolves a cached command token first, then env → vault → file → command; sealed types mint.
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
	if v, ok := cachedCommand(p.Name); ok && p.Auth.Command != "" { // a refreshed command token outranks static sources
		return v, nil
	}
	v, vaultErr := vaultCredential(p)
	if v != "" {
		return v, nil
	}
	if v := fileCredential(p); v != "" {
		return v, nil
	}
	if p.Auth.Command != "" {
		return commandCredential(ctx, p, false)
	}
	if vaultErr != nil {
		return "", fmt.Errorf("%s: no credential (vault: %v)", p.Name, vaultErr)
	}
	return "", fmt.Errorf("%s: no credential (set %s with `claude-code vault set`%s)", p.Name, orDash(p.Auth.Env), hint(p))
}

// vaultCredential returns the first non-empty effective secret among the key names, plus the first vault error seen.
func vaultCredential(p *Provider) (string, error) {
	var vaultErr error
	for _, n := range keyNames(p) {
		v, _, err := vault.EffectiveSecret(n)
		vaultErr = cmp.Or(vaultErr, err)
		if err == nil && v != "" {
			return v, vaultErr
		}
	}
	return "", vaultErr
}

// fileCredential returns the trimmed contents of auth.file, or "" when unset, unreadable or blank.
func fileCredential(p *Provider) string {
	if p.Auth.File == "" {
		return ""
	}
	b, err := os.ReadFile(paths.ExpandHome(p.Auth.File))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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
	done     chan struct{}
	force    bool
	val      string
	err      error
	panicked any // the mint's panic value, re-raised in the leader
}

// errMintAborted is what followers see when the leader's mint panics before it returns.
var errMintAborted = errors.New("credential mint aborted")

// flightHook is a test seam: called with true when a mint leader starts, false when a follower starts waiting.
var flightHook = func(bool) {}

// flightGroup is single-flight per provider name; cached short-circuits a non-forced call before it joins a flight.
type flightGroup struct {
	mu      sync.Mutex
	flights map[string]*flight
	cached  func(name string) (string, bool)
}

var cmdFlights = &flightGroup{flights: map[string]*flight{}, cached: cachedCommand}

func cachedCommand(name string) (string, bool) {
	if c, ok := cmdCache.Load(name); ok && time.Now().Before(c.(cached).until) {
		return c.(cached).value, true
	}
	return "", false
}

// mintOnce is the one mint coordinator: per provider, single-flight; a forced call never joins a non-forced flight.
func mintOnce(ctx context.Context, p *Provider, force bool, mint func(context.Context) (string, error)) (string, error) {
	return cmdFlights.do(ctx, p.Name, force, mint)
}

func (g *flightGroup) do(ctx context.Context, name string, force bool, mint func(context.Context) (string, error)) (string, error) {
	for {
		g.mu.Lock()
		if !force {
			if v, ok := g.cached(name); ok {
				g.mu.Unlock()
				return v, nil
			}
		}
		f, inflight := g.flights[name]
		if !inflight {
			f = &flight{done: make(chan struct{}), force: force, err: errMintAborted}
			g.flights[name] = f
			g.mu.Unlock()
			flightHook(true)
			go g.runMint(name, f, func() { f.val, f.err = mint(context.WithoutCancel(ctx)) })
			return awaitFlight(ctx, f)
		}
		g.mu.Unlock()
		flightHook(false)
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

// runMint runs one flight's mint, then retires the flight and wakes its waiters, even if the leader gave up.
func (g *flightGroup) runMint(name string, f *flight, mint func()) {
	defer func() {
		f.panicked = recover() // re-raised in the leader's caller by awaitFlight
		g.mu.Lock()
		delete(g.flights, name)
		g.mu.Unlock()
		close(f.done)
	}()
	mint()
}

// awaitFlight waits for f's mint; a caller whose ctx ends stops waiting while the mint continues for followers.
func awaitFlight(ctx context.Context, f *flight) (string, error) {
	select {
	case <-f.done:
		if f.panicked != nil {
			panic(f.panicked)
		}
		return f.val, f.err
	case <-ctx.Done():
		return "", ctx.Err()
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
	if rotateNearExpiry(p, cmd, v) {
		if v, err = runAuthCommand(ctx, p, p.Auth.RefreshCommand); err != nil {
			return "", err
		}
	}
	exp := time.Time{}
	if e, ok := JWTExp(v); ok {
		exp = time.Unix(e, 0)
	}
	if err := finishCredential(p, v, exp); err != nil {
		return "", err
	}
	return v, nil
}

// rotateNearExpiry reports whether v came from the primary command, is near expiry and a refresh command exists.
func rotateNearExpiry(p *Provider, cmd, v string) bool {
	exp, ok := JWTExp(v)
	return ok && p.Auth.RefreshCommand != "" && cmd != p.Auth.RefreshCommand &&
		time.Until(time.Unix(exp, 0)) < expiryMargin
}

// finishCredential is the shared tail of every mint: issuer check, expiry record, cache until min(ttl, exp-10m).
func finishCredential(p *Provider, v string, exp time.Time) error {
	if err := checkIssuer(p, v); err != nil {
		return err
	}
	recordTokenExpiry(p.Name, v, expiryMethod(p.Auth.Method()))
	ttl := time.Duration(p.Auth.TTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	until := time.Now().Add(ttl)
	if exp.IsZero() {
		if e, ok := JWTExp(v); ok {
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

// redactPunct is the quoting and separator punctuation around a token in JSON or key=value stderr.
const redactPunct = "\"'`{}[](),;:="

// maskCore replaces the bare token in w with mask, keeping the quotes and separators around it.
func maskCore(w, mask string) string {
	core := strings.Trim(w, redactPunct)
	if core == "" {
		return w
	}
	return strings.Replace(w, core, mask, 1)
}

// redactDetail is stderr's last non-empty line, ≤160 chars, with credential-shaped words masked.
func redactDetail(stderr string) string {
	lines := strings.FieldsFunc(stderr, func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) == 0 {
		return ""
	}
	words := strings.Fields(textutil.RedactSecrets(lines[len(lines)-1]))
	for i, w := range words {
		core := strings.Trim(w, redactPunct)
		key := strings.ToLower(core)
		if i+1 < len(words) && slices.Contains([]string{"token", "access_token", "bearer", "authorization"}, key) {
			words[i+1] = maskCore(words[i+1], "REDACTED")
		}
		if strings.HasPrefix(core, "eyJ") { // a JWT; textutil.LooksSecret covers the prefixed key families
			words[i] = maskCore(w, "REDACTED")
		}
	}
	return textutil.CapRunes(strings.Join(words, " "), 160, "…")
}

// CredentialPresent checks configured credential sources without decrypting or running commands.
func CredentialPresent(p *Provider) bool {
	switch p.Auth.Type {
	case AuthNone, AuthCLI:
		return true
	}
	if sealedAuth(p.Auth.Type) {
		return sealedPresent(p)
	}
	if p.Auth.Command != "" {
		return true
	}
	if slices.ContainsFunc(keyNames(p), func(n string) bool { return HasSecret(n) != "" }) {
		return true
	}
	if p.Auth.File != "" {
		b, err := os.ReadFile(paths.ExpandHome(p.Auth.File))
		return err == nil && strings.TrimSpace(string(b)) != ""
	}
	return false
}

// sealedPresent reports a stored credential authCredential would mint from (user secret, or the SP's per identity).
func sealedPresent(p *Provider) bool {
	mode, _ := ResolveIdentity(p.Identity, "")
	if p.Auth.Type == AuthOAuth && mode == IdentitySP {
		return spPresent(p)
	}
	if HasSecret(AuthSecretName(p)) != "" {
		return true
	}
	return p.Auth.Type == AuthOAuth && mode == IdentityAuto && spPresent(p)
}

// spPresent reports a paired service principal: its spClientId and a stored M2M secret.
func spPresent(p *Provider) bool {
	return p.Auth.SPClientID != "" && HasSecret(SecretName(p.provider(), p.Name, AuthM2M)) != ""
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

// CredentialNames are the env/vault names p's static credential may be read from, sealed (PAT/M2M) names included.
func CredentialNames(p *Provider) []string {
	out := keyNames(p)
	if n := AuthSecretName(p); n != "" {
		out = append(out, n)
	}
	return out
}
