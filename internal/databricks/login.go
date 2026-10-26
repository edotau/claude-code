package databricks

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/databricks/sdkport"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/profile"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault"
)

// loginU2M is the browser login, a var so tests never open a browser.
var loginU2M = providers.LoginU2M

// fail prints one error line for the verb and returns exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "claude-code databricks "+format+"\n", a...)
	return 1
}

var loginPage = helptext.Page{
	Usage: "claude-code databricks login [--profile <name>] [flags]",
	About: "Sign in through the browser for a provider row with auth.authType u2m;\n" +
		"the refresh token is sealed in the vault.",
}

// login runs the u2m browser login for a provider row and seals its refresh token.
func login(args []string) int {
	fs := helptext.NewFlagSet("databricks login", loginPage)
	dryRun := fs.Bool("dry-run", false, "report what would happen; open no browser, write nothing")
	authType := fs.String("auth-type", providers.AuthU2M, "switch the row's auth.authType first (only u2m)")
	fs.StringVar(authType, "t", providers.AuthU2M, "shorthand for --auth-type")
	profileName := profileFlag(fs)
	names, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) > 0 {
		return usageProfile("login", names[0])
	}
	if *authType != providers.AuthU2M {
		fail("login: --auth-type %q does not sign in through the browser; "+
			"oauth-m2m and pat secrets come from `vault set` or `databricks import --seal-secrets`", *authType)
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("login: %v", err)
	}
	p, err := reg.Get(resolveProfile(reg, *profileName))
	if err != nil {
		return fail("login: %v", err)
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "auth-type" || f.Name == "t" })
	if t := p.Auth.AuthType; t != "" && t != providers.AuthU2M && !explicit {
		return fail("login: %s has auth.authType %q; pass --auth-type u2m to switch it to a browser login", p.Name, t)
	}
	if *dryRun {
		if p.Auth.AuthType != providers.AuthU2M {
			if err := switchAuthType(p, providers.AuthU2M, true); err != nil {
				return fail("login: %v", err)
			}
			fmt.Printf("would log in to %s\n", loginHost(p))
			return 0
		}
		fmt.Printf("would open a browser login for %s and seal the refresh token as %s; nothing written\n",
			p.Name, providers.AuthSecretName(p))
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := LoginRow(ctx, p.Name, os.Stdout); err != nil {
		return fail("login: %v", err)
	}
	return 0
}

// LoginRow runs the u2m browser login for the named row, switching its authType to u2m first if needed.
func LoginRow(ctx context.Context, name string, out io.Writer) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p, err := reg.Get(name)
	if err != nil {
		return err
	}
	switched, was := false, p.Auth.AuthType
	if was != providers.AuthU2M {
		if err := switchAuthType(p, providers.AuthU2M, false); err != nil {
			return err
		}
		p.Auth.AuthType = providers.AuthU2M
		switched = true
		defer func() { rollbackHint(p.Name, was, switched) }()
	}
	ttl, err := loginU2M(ctx, p, sdkport.LoginOptions{Open: openLoginURL, Out: os.Stderr})
	if err != nil {
		return err
	}
	switched = false
	_ = route.ClearProviderAuth(p.Name) // best-effort: the TTL backstops a missed clear
	fmt.Fprintf(out, "logged in · token expires in %dm\n", int(ttl.Minutes()))
	return nil
}

// openLoginURL hands the authorize URL to the desktop opener; an error makes the login print the URL instead.
func openLoginURL(url string) error {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	return exec.Command(bin, url).Start()
}

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
	fmt.Fprintf(os.Stderr, "auth.authType of %s is now u2m (was %q); to undo, run `claude-code vault edit` and set "+
		"providers.%s.auth.authType back, or copy overlay/providers.prev (the previous generation) over overlay/providers\n",
		id, was, id)
}

// switchAuthType sets providers.<id>.auth.authType (u2m, pat or oauth-m2m) through the force path and prints the diff.
func switchAuthType(p *providers.Provider, authType string, dryRun bool) error {
	switch authType {
	case providers.AuthU2M, providers.AuthPAT:
	case providers.AuthOAuthM2M:
		if p.Auth.ClientID == "" {
			return fmt.Errorf("%s: auth.authType %s needs auth.clientId; set it with `claude-code vault edit` first",
				p.Name, authType)
		}
	default:
		return fmt.Errorf("unsupported auth.authType %q (want u2m, pat or oauth-m2m)", authType)
	}
	var kept []string
	oldDoc, newDoc, err := providers.UpdateOverlayDocForce(!dryRun, true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		provs := map[string]map[string]json.RawMessage{}
		if b, ok := top["providers"]; ok {
			if err := json.Unmarshal(b, &provs); err != nil {
				return fmt.Errorf("vault overlay/providers: providers: %w", err)
			}
		}
		entry := provs[p.Name]
		if entry == nil {
			return fmt.Errorf("%s is not an overlay row; add it with `vault edit` first", p.Name)
		}
		auth := map[string]json.RawMessage{}
		if b, ok := entry["auth"]; ok {
			if err := json.Unmarshal(b, &auth); err != nil {
				return fmt.Errorf("%s: auth: %w", p.Name, err)
			}
		}
		auth["authType"] = json.RawMessage(`"` + authType + `"`)
		kept = dropDerivedCommands(p, auth)
		b, err := json.Marshal(auth)
		if err != nil {
			return err
		}
		entry["auth"] = b
		if top["providers"], err = json.Marshal(provs); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !profile.PrintDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("no overlay change")
	}
	for _, k := range kept {
		fmt.Printf("kept custom auth.%s (not derived from cliProfile)\n", k)
	}
	return nil
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

// profileFlag registers --profile (and its shorthand -p), the provider row a databricks command acts on.
func profileFlag(fs *flag.FlagSet) *string {
	v := fs.String("profile", "", "provider row (default: the pinned Databricks row, else HARNESS_PROVIDER's env, else qa)")
	fs.StringVar(v, "p", "", "shorthand for --profile")
	return v
}

// resolveProfile is name, else the vault default when it is a Databricks row, else envOrDefault's env.
func resolveProfile(reg *providers.Registry, name string) string {
	if name != "" {
		return name
	}
	if p := reg.Providers[reg.Default]; p != nil && p.Preset == providers.PresetDatabricks {
		return reg.Default
	}
	return envOrDefault("")
}

// usageProfile refuses a positional row name: commands take only flags (--profile).
func usageProfile(verb, arg string) int {
	fmt.Fprintf(os.Stderr, "claude-code databricks %s: unexpected argument %q; pass --profile %s\n", verb, arg, arg)
	return 2
}
