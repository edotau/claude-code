package databricks

import (
	"context"
	"encoding/json"
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
	p, undo, err := patRow(name)
	if err != nil {
		return "", err
	}
	secret := providers.AuthSecretName(p)
	old, had, err := vault.ReadSecret(secret)
	if err != nil {
		return "", errors.Join(fmt.Errorf("%s: read the previous personal access token: %w", p.Name, err), undo())
	}
	if err := vault.Store(secret, value); err != nil {
		return "", errors.Join(err, undo())
	}
	status, detail := probePAT(ctx, p)
	if status != "rejected" {
		return status, nil
	}
	if rbErr := rollbackPAT(secret, old, had, undo); rbErr != nil {
		return status, fmt.Errorf("%s: personal access token rejected (%s); rollback failed, the rejected value may remain: %w",
			p.Name, detail, rbErr)
	}
	return status, fmt.Errorf("%s: personal access token rejected (%s); kept the previous value", p.Name, detail)
}

// patRow loads the named row, switches it to pat auth when it is not already, and returns the reloaded row
// with an undo that restores its previous overlay auth block.
func patRow(name string) (*providers.Provider, func() error, error) {
	noop := func() error { return nil }
	reg, err := providers.Load()
	if err != nil {
		return nil, noop, err
	}
	p, err := reg.Get(name)
	if err != nil {
		return nil, noop, err
	}
	if p.Auth.Method() == providers.AuthPAT {
		return p, noop, nil
	}
	prevAuth, err := overlayAuth(name)
	if err != nil {
		return nil, noop, err
	}
	undo := func() error { return restoreOverlayAuth(name, prevAuth) }
	if err := switchAuthType(p, providers.AuthPAT, false); err != nil {
		return nil, noop, err
	}
	if reg, err = providers.Load(); err != nil {
		return nil, noop, errors.Join(err, undo())
	}
	if p, err = reg.Get(name); err != nil {
		return nil, noop, errors.Join(err, undo())
	}
	return p, undo, nil
}

// overlayAuth returns the raw auth block of name's overlay row (nil when it has none).
func overlayAuth(name string) (json.RawMessage, error) {
	var raw json.RawMessage
	_, _, err := providers.UpdateOverlayDocForce(false, true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		provs := map[string]map[string]json.RawMessage{}
		if b, ok := top["providers"]; ok {
			if err := json.Unmarshal(b, &provs); err != nil {
				return err
			}
		}
		raw = provs[name]["auth"]
		return nil
	})
	return raw, err
}

// restoreOverlayAuth writes raw back as name's overlay auth block, or removes the block when raw is nil.
func restoreOverlayAuth(name string, raw json.RawMessage) error {
	_, _, err := providers.UpdateOverlayDocForce(true, true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		provs := map[string]map[string]json.RawMessage{}
		if err := json.Unmarshal(top["providers"], &provs); err != nil {
			return err
		}
		entry := provs[name]
		if entry == nil {
			return fmt.Errorf("%s is not an overlay row", name)
		}
		if raw == nil {
			delete(entry, "auth")
		} else {
			entry["auth"] = raw
		}
		b, err := json.Marshal(provs)
		top["providers"] = b
		return err
	})
	return err
}

// probePAT refreshes p's credential and probes it; a refresh failure reports "unverified" with its message.
func probePAT(ctx context.Context, p *providers.Provider) (status, detail string) {
	if _, err := providers.Refresh(ctx, p); err != nil {
		return "unverified", err.Error()
	}
	return providers.ProbeCredential(ctx, p)
}

// rollbackPAT restores the previous secret and the row's previous auth block.
func rollbackPAT(secret, old string, had bool, undo func() error) error {
	var errs []error
	if had {
		errs = append(errs, vault.Store(secret, old))
	} else {
		errs = append(errs, vault.Delete(secret))
	}
	errs = append(errs, undo())
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
