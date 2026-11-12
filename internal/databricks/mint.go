package databricks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
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
	Usage: "claude-code databricks token [--profile <name>] [flags]",
	About: "Fetch a fresh access token for a profile (the router's own mint, forced) and report its " +
		"length and expiry. The token is never printed; --copy puts it on the clipboard. " +
		"--auth picks the credential: oauth (your login), m2m (the service principal) or pat (the stored PAT).",
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
	res := MintResult{Provider: p.Name, AuthType: mintedAuthType(p, tok), Len: len(tok)}
	if e, ok := providers.JWTExp(tok); ok { // the minted token's own exp, not the stored record
		res.Expiry = time.Unix(e, 0)
	} else if exp, ok := providers.TokenExpiry(p.Name); ok { // opaque token: the stored record's expiry
		res.Expiry = exp
	}
	return res, tok, nil
}

// mintedAuthType reads the kind from the token itself: a service principal's JWT has sub == client_id.
func mintedAuthType(p *providers.Provider, tok string) string {
	var c struct {
		Sub      string `json:"sub"`
		ClientID string `json:"client_id"`
	}
	if textutil.JWTClaims(tok, &c) && c.Sub != "" && c.Sub == c.ClientID {
		return providers.AuthM2M
	}
	return authLabel(p)
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
	if status != providers.ProbeRejected {
		return status, nil
	}
	if rbErr := rollbackPAT(secret, old, had, undo); rbErr != nil {
		return status, fmt.Errorf("%s: personal access token rejected (%s); rollback failed, the rejected value may remain: %w",
			p.Name, detail, rbErr)
	}
	return status, fmt.Errorf("%s: personal access token rejected (%s); kept the previous value", p.Name, detail)
}

// patRow loads the row, switches it to pat auth if needed, and returns it with an undo for the previous auth block.
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
	if err := switchAuthTypeTo(io.Discard, p, providers.AuthPAT, false); err != nil {
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

// overlayProviders decodes the overlay's providers rows; a missing key is an empty map.
func overlayProviders(top map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	provs := map[string]map[string]json.RawMessage{}
	if b, ok := top["providers"]; ok {
		if err := json.Unmarshal(b, &provs); err != nil {
			return nil, err
		}
	}
	return provs, nil
}

// overlayAuth returns the raw auth block of name's overlay row (nil when it has none).
func overlayAuth(name string) (json.RawMessage, error) {
	var raw json.RawMessage
	_, _, err := providers.UpdateOverlayDocForce(false, true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		provs, err := overlayProviders(top)
		raw = provs[name]["auth"]
		return err
	})
	return raw, err
}

// restoreOverlayAuth writes raw back as name's overlay auth block, or removes the block when raw is nil.
func restoreOverlayAuth(name string, raw json.RawMessage) error {
	_, _, err := providers.UpdateOverlayDocForce(true, true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		provs, err := overlayProviders(top)
		if err != nil {
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
		return providers.ProbeUnverified, err.Error()
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

// authKinds maps --auth to the identity it mints as; pat has none, the profile itself must be a PAT row.
var authKinds = map[string]string{providers.AuthOAuth: providers.IdentityUser, providers.AuthM2M: providers.IdentitySP, providers.AuthPAT: ""}

// authIdentity resolves --auth against p and --as: the identity to mint as, or why p cannot serve that kind.
func authIdentity(p *providers.Provider, auth, as string) (string, error) {
	if auth == "" {
		return as, nil
	}
	identity, known := authKinds[auth]
	if !known {
		return "", fmt.Errorf("--auth must be oauth, m2m or pat (got %q)", auth)
	}
	if as != "" {
		return "", errors.New("--auth and --as both pick the credential; pass one")
	}
	if err := kindServed(p, auth); err != nil {
		return "", err
	}
	if p.Auth.Type != providers.AuthOAuth {
		return "", nil // an m2m or pat row has one credential: nothing to choose
	}
	return identity, nil
}

// kindServed is why p has no credential of kind auth, or nil when it has one.
func kindServed(p *providers.Provider, auth string) error {
	switch {
	case auth == providers.AuthPAT && p.Auth.Type != providers.AuthPAT:
		return fmt.Errorf("%s has no PAT (its auth type is %s)", p.Name, authLabel(p))
	case auth == providers.AuthOAuth && p.Auth.Type != providers.AuthOAuth:
		return fmt.Errorf("%s has no user login (its auth type is %s)", p.Name, authLabel(p))
	case auth == providers.AuthM2M && p.Auth.Type != providers.AuthM2M && !providers.SPPresent(p):
		return fmt.Errorf("%s has no service principal (pair one in `claude-code configure databricks` → Identity)", p.Name)
	}
	return nil
}

// mintFlags is `databricks token`'s parsed command line.
type mintFlags struct {
	dryRun, doCopy    bool
	clearAfter        time.Duration
	profile, as, auth string
}

// parseMint binds and parses token's flags; done reports the exit code to return instead of minting.
func parseMint(args []string) (f mintFlags, code int, done bool) {
	fs := helptext.NewFlagSet("databricks token", mintPage)
	fs.BoolVar(&f.dryRun, "dry-run", false, "report what would happen; mint nothing")
	fs.BoolVar(&f.doCopy, "copy", false, "put the token on the clipboard, then clear it")
	fs.DurationVar(&f.clearAfter, "clear-after", clipboard.DefaultClearAfter, "with --copy: clear the clipboard after this long")
	profileFlagVar(fs, &f.profile, profileUsage)
	fs.StringVar(&f.as, "as", "", "identity to mint as: user|sp (default: the profile's identity setting)")
	fs.StringVar(&f.auth, "auth", "", "credential kind to fetch: oauth|m2m|pat (default: the profile's own)")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return f, helptext.ExitCode(err), true
	}
	if len(names) > 0 {
		return f, usageProfile("token", names[0]), true
	}
	if err := checkProfileFlag(fs, f.profile); err != nil {
		fail("token: %v", err)
		return f, 2, true
	}
	if _, err := providers.ResolveIdentity("", f.as); err != nil || f.as == providers.IdentityAuto {
		return f, fail("token: --as must be user or sp"), true
	}
	return f, 0, false
}

// mint is `databricks token [--profile <name>] [--auth kind] [--copy] [--clear-after d] [--dry-run]`.
func mint(args []string) int {
	f, code, done := parseMint(args)
	if done {
		return code
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("token: %v", err)
	}
	name := resolveProfile(reg, f.profile)
	if name == "" {
		return fail("token: %v", errNoProfile)
	}
	if p, err := reg.Get(name); err == nil {
		if f.as, err = authIdentity(p, f.auth, f.as); err != nil {
			return fail("token: %v", err)
		}
	}
	if f.dryRun {
		return mintDryRun(reg, name, f.as, f.doCopy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, tok, err := Mint(providers.WithIdentity(ctx, f.as), name)
	if err != nil {
		return fail("token: %v", err)
	}
	fmt.Println(mintLine(res))
	if !f.doCopy {
		return 0
	}
	return copyToken(ctx, tok, f.clearAfter)
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
		return fail("token: copy: %v", err)
	}
	if err := clipboard.ScheduleClear(after, tok); err != nil {
		if _, cerr := clipboard.ClearIfMatch(ctx, clipboard.Digest(tok)); cerr != nil {
			return fail("token: copied, the timed clear failed (%v) and so did clearing now: %v", err, cerr)
		}
		return fail("token: the timed clear failed (%v); cleared the clipboard now", err)
	}
	fmt.Printf("copied · clears in %s\n", after)
	return 0
}

// mintDryRun reports what mint would do for name; it mints nothing and writes nothing.
func mintDryRun(reg *providers.Registry, name, as string, doCopy bool) int {
	p, err := reg.Get(name)
	if err != nil {
		return fail("token: %v", err)
	}
	if as == "" {
		as = "the profile's identity"
	}
	fmt.Printf("would mint a fresh token for %s (%s) as %s; copy=%t; nothing written\n", p.Name, authLabel(p), as, doCopy)
	return 0
}
