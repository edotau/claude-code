package databricks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
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
	res := MintResult{Provider: p.Name, AuthType: p.Auth.Method(), Len: len(tok)}
	if res.AuthType == "" {
		res.AuthType = "command"
	}
	if exp, ok := providers.TokenExpiry(p.Name); ok {
		res.Expiry = exp
	}
	return res, tok, nil
}

// SetPAT seals value as the row's PAT (switching authType to pat) and probes it; a rejected PAT is rolled back.
func SetPAT(ctx context.Context, name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty personal access token")
	}
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	p, err := reg.Get(name)
	if err != nil {
		return "", err
	}
	if p.Auth.Method() != providers.AuthPAT {
		if err := switchAuthType(p, providers.AuthPAT, false); err != nil {
			return "", err
		}
		if reg, err = providers.Load(); err != nil {
			return "", err
		}
		if p, err = reg.Get(name); err != nil {
			return "", err
		}
	}
	secret := providers.AuthSecretName(p)
	old, had, _ := vault.ReadSecret(secret)
	if err := secrets.Store(secret, value); err != nil {
		return "", err
	}
	status, detail := "unverified", ""
	if _, err := providers.Refresh(ctx, p); err == nil {
		status, detail = providers.ProbeCredential(ctx, p)
	} else {
		detail = err.Error()
	}
	if status != "rejected" {
		return status, nil
	}
	if had {
		_ = secrets.Store(secret, old)
	} else {
		_ = secrets.Delete(secret)
	}
	return status, fmt.Errorf("%s: personal access token rejected (%s); kept the previous value", p.Name, detail)
}

// mint is `databricks mint [--profile <name>] [--copy] [--clear-after d] [--dry-run]`.
func mint(args []string) int {
	fs := helptext.NewFlagSet("databricks mint", mintPage)
	dryRun := fs.Bool("dry-run", false, "report what would happen; mint nothing")
	doCopy := fs.Bool("copy", false, "put the token on the clipboard, then clear it")
	clearAfter := fs.Duration("clear-after", clipboard.DefaultClearAfter, "with --copy: clear the clipboard after this long")
	profileName := profileFlag(fs)
	names, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) > 0 {
		return usageProfile("mint", names[0])
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("mint: %v", err)
	}
	name := resolveProfile(reg, *profileName)
	if *dryRun {
		p, err := reg.Get(name)
		if err != nil {
			return fail("mint: %v", err)
		}
		fmt.Printf("would mint a fresh token for %s (%s); copy=%t; nothing written\n", p.Name, authLabel(p), *doCopy)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, tok, err := Mint(ctx, name)
	if err != nil {
		return fail("mint: %v", err)
	}
	fmt.Println(mintLine(res))
	if !*doCopy {
		return 0
	}
	return copyToken(ctx, tok, *clearAfter)
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
