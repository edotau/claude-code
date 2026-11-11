package databricks

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// loginU2M is the browser login, a var so tests never open a browser.
var loginU2M = providers.LoginU2M

// validU2M is the cached-login check, a var so tests need no sealed token.
var validU2M = providers.ValidU2M

// loginIn is the stdin --paste reads the redirected URL from, a var so tests inject one.
var loginIn io.Reader = os.Stdin

// headless holds the three opt-in U2M flags for logins without a local browser.
type headless struct {
	port      int
	noBrowser bool
	paste     bool
}

func (h headless) set() bool { return h.port != 0 || h.noBrowser || h.paste }

// options is the oauth.LoginOptions for h: no flag set gives the interactive default.
func (h headless) options() oauth.LoginOptions {
	o := oauth.LoginOptions{Open: openLoginURL, Out: os.Stderr, NoBrowser: h.noBrowser}
	if h.noBrowser {
		o.Open = nil
	}
	if h.port > 0 {
		o.Port, o.ExactPort = h.port, true
	}
	if h.paste {
		o.Paste = loginIn
	}
	return o
}

// describe is the dry-run line for h.
func (h headless) describe() string {
	port := "callback port 8030 (next free port up to 8050 if busy)"
	if h.port > 0 {
		port = fmt.Sprintf("callback port %d (exact; error if busy)", h.port)
	}
	browser, paste := "on", "off"
	if h.noBrowser {
		browser = "off"
	}
	if h.paste {
		paste = "on"
	}
	return fmt.Sprintf("%s; browser %s; paste %s", port, browser, paste)
}

// fail prints one error line for the verb and returns exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "claude-code databricks "+format+"\n", a...)
	return 1
}

var loginPage = helptext.Page{
	Usage: "claude-code databricks login [--profile <name>] [flags]",
	About: "Sign in through the browser for a profile with auth type oauth;\n" +
		"the refresh token is sealed in the vault.\n" +
		"A profile with a paired service principal mints as it, no browser; --as user forces the browser login.",
}

// login runs the u2m browser login for a provider row and seals its refresh token.
func login(args []string) int {
	fs := helptext.NewFlagSet("databricks login", loginPage)
	dryRun := fs.Bool("dry-run", false, "report what would happen; open no browser, write nothing")
	force := fs.Bool("force", false, "log in again even when the cached token is still valid")
	authType := fs.String("auth-type", "", "auth type to switch the profile to: only oauth is accepted (needed to change a non-oauth profile)")
	fs.StringVar(authType, "t", "", "shorthand for --auth-type")
	profileName := profileFlag(fs)
	as := fs.String("as", "", "identity: user (browser login, no SP fallback) or sp (service-principal mint, no browser)")
	var h headless
	fs.IntVar(&h.port, "port", 0, "exact localhost callback port to listen on, error if busy (default: 8030, next free up to 8050)")
	fs.BoolVar(&h.noBrowser, "no-browser", false, "do not open a browser; print the URL and the ssh -L forward hint")
	fs.BoolVar(&h.paste, "paste", false, "also accept the redirected URL pasted on stdin when the callback cannot be forwarded")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) > 0 {
		return usageProfile("login", names[0])
	}
	if code, bad := checkLoginFlags(*as, *authType); bad {
		return code
	}
	if code, bad := checkHeadless(h, *as); bad {
		return code
	}
	p, err := loginProfile(fs, *profileName)
	if err != nil {
		return fail("login: %v", err)
	}
	mode, err := providers.ResolveIdentity(p.Identity, *as)
	if err != nil {
		return fail("login: %s: %v", p.Name, err)
	}
	if mode == providers.IdentitySP {
		if code, bad := checkHeadless(h, providers.IdentitySP); bad {
			return code
		}
		return loginSP(p, *dryRun)
	}
	if mode == providers.IdentityAuto && *as == "" && providers.SPPresent(p) { // paired: no browser needed, flags unused
		return loginSP(p, *dryRun)
	}
	return loginUser(p, mode, h, *dryRun, !*force && p.Auth.Method() == providers.AuthOAuth)
}

// identityNote says what a browser login means for each identity login opens it for.
var identityNote = map[string]string{
	providers.IdentityUser: "browser login",
	providers.IdentityAuto: "browser login (no service principal is paired); launches use it once stored",
}

// loginUser is the u2m browser login for identity user or auto; reuse keeps a still-valid cached login.
func loginUser(p *providers.Provider, mode string, h headless, dryRun, reuse bool) int {
	if dryRun {
		fmt.Printf("identity %s: %s\n", mode, identityNote[mode])
		if h.set() {
			fmt.Println("would use " + h.describe())
		}
		return loginDryRun(p, reuse, h.noBrowser)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if reuse {
		if left, ok := validU2M(ctx, p); ok {
			fmt.Printf("already logged in · expires in %dm\n", int(left.Minutes()))
			return 0
		}
	}
	if err := loginRow(ctx, p.Name, os.Stdout, h); err != nil {
		return fail("login: %v", err)
	}
	return 0
}

// loginSP mints p's paired service-principal token through the router's forced mint; no browser, never the token.
func loginSP(p *providers.Provider, dryRun bool) int {
	secret := providers.SecretName(p.Provider, p.Name, providers.AuthM2M)
	if p.Auth.SPClientID == "" {
		return fail("login: %s has no paired service principal (auth.spClientId); pair one with `claude-code vault edit`, "+
			"or pass --as user for a browser login", p.Name)
	}
	if providers.HasSecret(secret) == "" {
		return fail("login: %s: no service-principal secret; run `claude-code vault set %s`", p.Name, secret)
	}
	if dryRun {
		fmt.Printf("identity sp: would mint a token for %s as the service principal %s; no browser, nothing written\n",
			p.Name, p.Auth.SPClientID)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = providers.WithNotes(providers.WithIdentity(ctx, providers.IdentitySP), func(string) {}) // the line below says it
	tok, err := providers.Refresh(ctx, p)
	if err != nil {
		if oauth.IsRejected(err) {
			return fail("login: %v; replace the secret with `claude-code vault set %s`", err, secret)
		}
		return fail("login: %v", err)
	}
	line := fmt.Sprintf("%s · service principal %s · token minted", p.Name, p.Auth.SPClientID)
	if e, ok := providers.JWTExp(tok); ok {
		line += fmt.Sprintf(" · expires in %dm", max(0, int(time.Until(time.Unix(e, 0)).Minutes())))
	}
	fmt.Println(line)
	return 0
}

// loginProfile resolves the profile to log in, refusing a non-oauth one unless --auth-type was passed.
func loginProfile(fs *flag.FlagSet, profileName string) (*providers.Provider, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	name := resolveProfile(reg, profileName)
	if name == "" {
		return nil, errNoProfile
	}
	p, err := reg.Get(name)
	if err != nil {
		return nil, err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "auth-type" || f.Name == "t" })
	if t := p.Auth.Method(); t != "" && t != providers.AuthOAuth && !explicit {
		return nil, fmt.Errorf("%s has auth type %q and signs in without a browser; use `login -p <oauth profile>`, "+
			"or pass --auth-type oauth to switch this row to a browser login", p.Name, t)
	}
	return p, nil
}

// checkLoginFlags rejects an --as or --auth-type value login cannot honor; bad reports the exit code to return.
func checkLoginFlags(as, authType string) (code int, bad bool) {
	switch as {
	case "", providers.IdentityUser, providers.IdentitySP:
	default:
		fail("login: --as must be user or sp")
		return 2, true
	}
	switch authType {
	case "", providers.AuthOAuth:
	case providers.AuthM2M, providers.AuthPAT:
		fail("login: --auth-type %q does not sign in through the browser; "+
			"m2m and pat secrets come from `vault set` or `databricks import --seal-secrets`", authType)
		return 2, true
	default:
		fail("login: --auth-type must be oauth, m2m or pat (got %q)", authType)
		return 2, true
	}
	return 0, false
}

// checkHeadless rejects --port, --no-browser and --paste with identity sp; a non-oauth auth type is refused earlier.
func checkHeadless(h headless, as string) (code int, bad bool) {
	if h.port < 0 || h.port > 65535 {
		fail("login: --port must be between 1 and 65535 (got %d)", h.port)
		return 2, true
	}
	if !h.set() {
		return 0, false
	}
	if as == providers.IdentitySP {
		fail("login: --port, --no-browser and --paste apply to the browser login only; " +
			"a service principal (--as sp) has no callback")
		return 2, true
	}
	return 0, false
}

// loginDryRun reports what login would do for p without opening a browser or writing anything.
func loginDryRun(p *providers.Provider, reuse, noBrowser bool) int {
	if reuse {
		if exp, ok := providers.TokenExpiry(p.Name); ok && time.Until(exp) > 0 {
			fmt.Printf("would reuse the cached token for %s; no browser\n", p.Name)
			return 0
		}
	}
	if p.Auth.Method() != providers.AuthOAuth {
		if err := switchAuthType(p, providers.AuthOAuth, true); err != nil {
			return fail("login: %v", err)
		}
		fmt.Printf("would log in to %s\n", loginHost(p))
		return 0
	}
	how := "open a browser login"
	if noBrowser {
		how = "print the sign-in URL"
	}
	fmt.Printf("would %s for %s and seal the refresh token as %s; nothing written\n", how, p.Name, providers.AuthSecretName(p))
	return 0
}

// LoginRow runs the u2m browser login for the named row, switching its authType to oauth first if needed.
func LoginRow(ctx context.Context, name string, out io.Writer) error {
	return loginRow(ctx, name, out, headless{})
}

// loginRow is LoginRow with the headless flags h.
func loginRow(ctx context.Context, name string, out io.Writer, h headless) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p, err := reg.Get(name)
	if err != nil {
		return err
	}
	switched, was := false, p.Auth.Method()
	if was != providers.AuthOAuth {
		if err := switchAuthType(p, providers.AuthOAuth, false); err != nil {
			return err
		}
		p.Auth.Type = providers.AuthOAuth
		switched = true
		defer func() { rollbackHint(p.Name, was, switched) }()
	}
	ttl, err := loginU2M(ctx, p, h.options())
	if err != nil {
		return err
	}
	switched = false
	if err := router.ClearProviderAuth(p.Name); err != nil { // the TTL backstops a missed clear
		fmt.Fprintf(os.Stderr, "warning: clear the router's re-auth mark for %s: %v\n", p.Name, err)
	}
	fmt.Fprintf(out, "logged in · token expires in %dm\n", int(ttl.Minutes()))
	return nil
}

// openLoginURL hands the authorize URL to the desktop opener (the Windows browser on WSL); the URL is printed too.
func openLoginURL(url string) error { return proc.OpenURL(url) }

// loginHost is the workspace a login would talk to, for display.
func loginHost(p *providers.Provider) string {
	if p.Host != "" {
		return p.Host
	}
	return p.BaseURL
}

// rollbackHint tells how to undo the authType switch when the login after it failed.
func rollbackHint(id, was string, switched bool) {
	if !switched {
		return
	}
	fmt.Fprintf(os.Stderr, "auth.type of %s is now oauth (was %q); to undo, run `claude-code vault edit` and set "+
		"providers.%s.auth.type back, or copy overlay/providers.prev (the previous generation) over overlay/providers\n",
		id, was, id)
}

// switchAuthType sets providers.<id>.auth.type (oauth, pat or m2m) through the force path and prints the diff.
func switchAuthType(p *providers.Provider, authType string, dryRun bool) error {
	if err := checkAuthType(p, authType); err != nil {
		return err
	}
	var kept []string
	oldDoc, newDoc, err := providers.UpdateOverlayDocForce(!dryRun, true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		var err error
		kept, err = setOverlayAuthType(top, p, authType)
		return err
	})
	if err != nil {
		return err
	}
	if !printDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("no overlay change")
	}
	for _, k := range kept {
		fmt.Printf("kept custom auth.%s (not derived from cliProfile)\n", k)
	}
	return nil
}

// checkAuthType refuses an auth type the switch cannot write: unknown, or m2m without a client id.
func checkAuthType(p *providers.Provider, authType string) error {
	switch authType {
	case providers.AuthOAuth, providers.AuthPAT:
	case providers.AuthM2M:
		if p.Auth.ClientID == "" {
			return fmt.Errorf("%s: auth type %s needs auth.clientId; set it with `claude-code vault edit` first",
				p.Name, authType)
		}
	default:
		return fmt.Errorf("unsupported auth type %q (want oauth, pat or m2m)", authType)
	}
	return nil
}

// setOverlayAuthType writes auth.type into p's overlay row in top and returns the custom auth keys it kept.
func setOverlayAuthType(top map[string]json.RawMessage, p *providers.Provider, authType string) ([]string, error) {
	provs, err := overlayProviders(top)
	if err != nil {
		return nil, fmt.Errorf("vault overlay/providers: providers: %w", err)
	}
	entry := provs[p.Name]
	if entry == nil {
		return nil, fmt.Errorf("%s is not an overlay row; add it with `vault edit` first", p.Name)
	}
	auth := map[string]json.RawMessage{}
	if b, ok := entry["auth"]; ok {
		if err := json.Unmarshal(b, &auth); err != nil {
			return nil, fmt.Errorf("%s: auth: %w", p.Name, err)
		}
	}
	auth["type"] = json.RawMessage(`"` + authType + `"`)
	kept := dropDerivedCommands(p, auth)
	b, err := json.Marshal(auth)
	if err != nil {
		return kept, err
	}
	entry["auth"] = b
	top["providers"], err = json.Marshal(provs)
	return kept, err
}

// dropDerivedCommands removes auth.command/refreshCommand equal to the preset's cliProfile derivation; returns the kept keys.
func dropDerivedCommands(p *providers.Provider, auth map[string]json.RawMessage) []string {
	derived := map[string]string{}
	if p.Preset == providers.PresetDatabricks && p.CLIProfile != "" {
		derived["command"], derived["refreshCommand"] = providers.DatabricksCLICommands(p.CLIProfile)
	}
	var kept []string
	for _, k := range []string{"command", "refreshCommand"} {
		raw, ok := auth[k]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && v == derived[k] {
			delete(auth, k)
		} else {
			kept = append(kept, k)
		}
	}
	return kept
}

// profileUsage is the --profile help shared by every command that resolves one profile.
const profileUsage = "Databricks profile (default: session, pinned, else the first)"

// profileFlag registers --profile (and its shorthand -p), the Databricks profile a command acts on.
func profileFlag(fs *flag.FlagSet) *string {
	v := new(string)
	profileFlagVar(fs, v, profileUsage)
	return v
}

// profileFlagVar binds --profile and -p to v; help shows them as one "-p, --profile NAME" row.
func profileFlagVar(fs *flag.FlagSet, v *string, usage string) {
	helptext.NameVar(fs, v, "profile", usage)
	helptext.NameVar(fs, v, "p", "shorthand for --profile")
}

// usageProfile refuses a positional profile name: commands take only flags (--profile).
func usageProfile(verb, arg string) int {
	fmt.Fprintf(os.Stderr, "claude-code databricks %s: unexpected argument %q; pass --profile %s\n", verb, arg, arg)
	return 2
}

var (
	SetRow = providers.SetProviderFields
)

// SaveIdentity writes env's identity mode and paired client id to its provider row, then seals secret (when given)
// as DATABRICKS_<ENV>_M2M, restoring the old one if the row write fails; never echoed. Mode sp needs a client id.
func SaveIdentity(reg *providers.Registry, env, mode, spID, secret string) error {
	p := reg.Providers[env]
	if p == nil {
		return fmt.Errorf("%s: no such profile", env)
	}
	if _, err := providers.ResolveIdentity(mode, ""); err != nil {
		return err
	}
	if mode == providers.IdentitySP && spID == "" {
		return fmt.Errorf("%s: identity sp needs a service principal client id", env)
	}
	name := providers.SecretName(providers.GroupDatabricks, env, providers.AuthM2M)
	var prev string
	var hadPrev bool
	if secret != "" {
		var err error
		if prev, hadPrev, err = vault.ReadSecret(name); err != nil {
			return err
		}
		if err := vault.Store(name, secret); err != nil {
			return err
		}
	}
	auth := p.Auth
	auth.SPClientID = spID
	if err := SetRow(env, map[string]any{"identity": mode, "auth": auth}); err != nil {
		if secret == "" {
			return err
		}
		return errors.Join(err, restoreSecret(name, secret, prev, hadPrev))
	}
	return nil
}

// restoreSecret puts record name back to prev (or deletes it) only while it still holds wrote; a newer value stays.
func restoreSecret(name, wrote, prev string, hadPrev bool) error {
	wrote = strings.TrimSpace(wrote)
	restored := false
	err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		rec, ok, err := tx.Record(name)
		if err != nil || !ok {
			return err
		}
		cur, err := key.Open(name, rec)
		if err != nil {
			return err
		}
		if cur != wrote {
			return nil
		}
		if !hadPrev {
			return tx.Put(vault.Secrets, name, "")
		}
		restored = true
		return tx.PutRecord(name, vault.Record{
			Sealed: key.Seal(name, prev), Kid: key.Kid, Prefix: vault.MetaPrefix(prev), Len: len(prev), SetAt: time.Now(),
		})
	})
	if err == nil && restored {
		audit.Log(audit.SecretSet, name, "vault") // vault.Store's audit line, which this write bypasses
	}
	return err
}
