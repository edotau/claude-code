package databricks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// MintResult describes a minted credential; it never carries the token itself.
type MintResult struct {
	Provider string
	AuthType string
	Expiry   time.Time // zero when the credential reports none
	Len      int
}

var mintPage = helptext.Page{
	Usage: "claude-code databricks mint [--profile <name>] [flags]",
	About: "Mint a fresh access token for a profile (the router's own mint, forced) and report its\n" +
		"length and expiry. The token is never printed; --copy puts it on the clipboard.",
}

// Mint forces a fresh credential for the named row; the token is returned in-process only.
func Mint(ctx context.Context, name string) (MintResult, string, error) {
	reg, err := providers.Load()
	if err != nil {
		return MintResult{}, "", err
	}
	p, err := reg.Get(name)
	if err != nil {
		return MintResult{}, "", err
	}
	tok, err := providers.Refresh(ctx, p)
	if err != nil {
		return MintResult{}, "", err
	}
	res := MintResult{Provider: p.Name, AuthType: authLabel(p), Len: len(tok)}
	if exp, ok := providers.TokenExpiry(p.Name); ok {
		res.Expiry = exp
	}
	return res, tok, nil
}

// SetPAT seals value as the row's PAT (switching authType to pat) and probes it; a rejected PAT is rolled back.
func SetPAT(ctx context.Context, name, value string) (status string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty personal access token")
	}
	lockErr := paths.WithFileLock(filepath.Join(paths.StateDir(), "databricks-pat-"+name), func() error {
		status, err = setPATLocked(ctx, name, value)
		return nil
	})
	if lockErr != nil {
		return "", fmt.Errorf("%s: lock the personal access token update: %w", name, lockErr)
	}
	return status, err
}

// setPATLocked is SetPAT's read, store, probe and rollback, run while the per-row lock is held.
func setPATLocked(ctx context.Context, name, value string) (string, error) {
	p, prevType, err := patRow(name)
	if err != nil {
		return "", err
	}
	secret := providers.AuthSecretName(p)
	old, had, err := vault.ReadSecret(secret)
	if err != nil {
		return "", fmt.Errorf("%s: read the previous personal access token: %w", p.Name, err)
	}
	if err := vault.Store(secret, value); err != nil {
		return "", err
	}
	status, detail := probePAT(ctx, p)
	if status != "rejected" {
		return status, nil
	}
	if rbErr := rollbackPAT(p, secret, old, had, prevType); rbErr != nil {
		return status, fmt.Errorf("%s: personal access token rejected (%s); rollback failed, the rejected value may remain: %w",
			p.Name, detail, rbErr)
	}
	return status, fmt.Errorf("%s: personal access token rejected (%s); kept the previous value", p.Name, detail)
}

// patRow loads the named row, switches it to pat auth when it is not already, and returns the reloaded row
// with its previous auth type.
func patRow(name string) (*providers.Provider, string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, "", err
	}
	p, err := reg.Get(name)
	if err != nil {
		return nil, "", err
	}
	prevType := p.Auth.Method()
	if prevType == providers.AuthPAT {
		return p, prevType, nil
	}
	if err := switchAuthType(p, providers.AuthPAT, false); err != nil {
		return nil, "", err
	}
	if reg, err = providers.Load(); err != nil {
		return nil, "", err
	}
	p, err = reg.Get(name)
	return p, prevType, err
}

// probePAT refreshes p's credential and probes it; a refresh failure reports "unverified" with its message.
func probePAT(ctx context.Context, p *providers.Provider) (status, detail string) {
	if _, err := providers.Refresh(ctx, p); err != nil {
		return "unverified", err.Error()
	}
	return providers.ProbeCredential(ctx, p)
}

// rollbackPAT restores the previous secret and, when it was oauth or m2m, the previous authType.
func rollbackPAT(p *providers.Provider, secret, old string, had bool, prevType string) error {
	var errs []error
	if had {
		errs = append(errs, vault.Store(secret, old))
	} else {
		errs = append(errs, vault.Delete(secret))
	}
	if prevType == providers.AuthOAuth || prevType == providers.AuthM2M {
		errs = append(errs, switchAuthType(p, prevType, false))
	}
	return errors.Join(errs...)
}

// mint is `databricks mint [--profile <name>] [--copy] [--clear-after d] [--dry-run]`.
func mint(args []string) int {
	fs := helptext.NewFlagSet("databricks mint", mintPage)
	dryRun := fs.Bool("dry-run", false, "report what would happen; mint nothing")
	doCopy := fs.Bool("copy", false, "put the token on the clipboard, then clear it")
	clearAfter := fs.Duration("clear-after", clipboard.DefaultClearAfter, "with --copy: clear the clipboard after this long")
	profileName := profileFlag(fs)
	as := fs.String("as", "", "identity to mint as: user|sp (default: the profile's identity setting)")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) > 0 {
		return usageProfile("mint", names[0])
	}
	if _, err := providers.ResolveIdentity("", *as); err != nil || *as == providers.IdentityAuto {
		return fail("mint: --as must be user or sp")
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("mint: %v", err)
	}
	name := resolveProfile(reg, *profileName)
	if name == "" {
		return fail("mint: %v", errNoProfile)
	}
	if *dryRun {
		return mintDryRun(reg, name, *as, *doCopy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, tok, err := Mint(providers.WithIdentity(ctx, *as), name)
	if err != nil {
		return fail("mint: %v", err)
	}
	fmt.Println(mintLine(res))
	if !*doCopy {
		return 0
	}
	return copyToken(ctx, tok, *clearAfter)
}

// asLabel names the identity a mint would use; empty --as means the profile's own setting.
func asLabel(as string) string {
	if as == "" {
		return "the profile's identity"
	}
	return as
}

func authLabel(p *providers.Provider) string {
	if m := p.Auth.Method(); m != "" {
		return m
	}
	return "command"
}

// mintLine is the one status line: provider, authType, length and time left; never the token.
func mintLine(r MintResult) string {
	line := fmt.Sprintf("%s · %s · minted · %d chars", r.Provider, r.AuthType, r.Len)
	if !r.Expiry.IsZero() {
		line += fmt.Sprintf(" · expires in %dm", max(0, int(time.Until(r.Expiry).Round(time.Minute).Minutes())))
	}
	return line
}

// copyToken writes tok to the clipboard and schedules the timed clear; the token never reaches argv or output.
func copyToken(ctx context.Context, tok string, after time.Duration) int {
	if err := clipboard.WriteSecret(ctx, tok); err != nil {
		if errors.Is(err, clipboard.ErrNoClipboard) {
			fmt.Fprintln(os.Stderr, "no clipboard here; the agents already fetch the token through the router")
			return 0
		}
		return fail("mint: copy: %v", err)
	}
	if err := clipboard.ScheduleClear(after, tok); err != nil {
		return fail("mint: copied, but the timed clear failed: %v", err)
	}
	fmt.Printf("copied · clears in %s\n", after)
	return 0
}

// mintDryRun reports what mint would do for name; it mints nothing and writes nothing.
func mintDryRun(reg *providers.Registry, name, as string, doCopy bool) int {
	p, err := reg.Get(name)
	if err != nil {
		return fail("mint: %v", err)
	}
	fmt.Printf("would mint a fresh token for %s (%s) as %s; copy=%t; nothing written\n", p.Name, authLabel(p), asLabel(as), doCopy)
	return 0
}
