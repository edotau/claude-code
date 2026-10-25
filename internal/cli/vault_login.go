package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/dbxoauth"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/profile"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// loginU2M is the browser login, a var so tests never open a browser.
var loginU2M = providers.LoginU2M

// cmdVaultLogin runs the databricks-cli browser login for a provider row and seals its refresh token.
func cmdVaultLogin(args []string) int {
	fs := helptext.NewFlagSet("vault login", helptext.Page{
		Usage: "claude-code vault login <provider> [flags]",
		About: "Sign in through the browser for a provider row with auth.authType databricks-cli;\n" +
			"the refresh token is sealed in the vault.",
	})
	dryRun := fs.Bool("dry-run", false, "report what would happen; open no browser, write nothing")
	authType := fs.String("auth-type", "", "switch the row's auth.authType first (only databricks-cli)")
	names, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) != 1 {
		fs.Usage()
		return 2
	}
	if *authType != "" && *authType != providers.AuthDatabricksCLI {
		fail("vault login: --auth-type %q does not sign in through the browser; "+
			"oauth-m2m and pat secrets come from `vault set` or `vault import --seal-secrets`", *authType)
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("vault login: %v", err)
	}
	p, err := reg.Get(names[0])
	if err != nil {
		return fail("vault login: %v", err)
	}
	switched := false
	if *authType == providers.AuthDatabricksCLI && p.Auth.AuthType != providers.AuthDatabricksCLI {
		was := p.Auth.AuthType
		if err := switchToDatabricksCLI(p, *dryRun); err != nil {
			return fail("vault login: %v", err)
		}
		if *dryRun {
			fmt.Printf("would log in to %s\n", loginHost(p))
			return 0
		}
		switched = true
		p.Auth.AuthType = providers.AuthDatabricksCLI
		defer func() { rollbackHint(p.Name, was, switched) }()
	}
	if p.Auth.AuthType != providers.AuthDatabricksCLI {
		return fail("vault login: %s has auth.authType %q; only databricks-cli rows sign in through the browser",
			p.Name, p.Auth.AuthType)
	}
	if *dryRun {
		fmt.Printf("would open a browser login for %s and seal the refresh token as %s; nothing written\n",
			p.Name, providers.AuthSecretName(p))
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ttl, err := loginU2M(ctx, p, dbxoauth.LoginOptions{Open: openLoginURL, Out: os.Stderr})
	if err != nil {
		return fail("vault login: %v", err)
	}
	switched = false
	fmt.Printf("logged in · token expires in %dm\n", int(ttl.Minutes()))
	return 0
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
	fmt.Fprintf(os.Stderr, "auth.authType of %s is now databricks-cli (was %q); to undo, run `claude-code vault edit` and set "+
		"providers.%s.auth.authType back, or copy overlay/providers.prev (the previous generation) over overlay/providers\n",
		id, was, id)
}

// switchToDatabricksCLI sets providers.<id>.auth.authType to databricks-cli through the force path and prints the overlay diff.
func switchToDatabricksCLI(p *providers.Provider, dryRun bool) error {
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
		auth["authType"] = json.RawMessage(`"` + providers.AuthDatabricksCLI + `"`)
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
