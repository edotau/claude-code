package menu

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
	"github.com/edotau/claude-code/internal/ui/tui"
	"slices"
	"time"
)

var (
	dbxLogin = databricks.LoginRow

	dbxMint = databricks.Mint

	dbxSetPAT = databricks.SetPAT

	dbxShell = func(d *dialog) *uidialog.Shell { return d.shell() }

	dbxSecret = tui.Secret

	dbxCopy = clipboard.WriteSecret

	dbxSchedule = clipboard.ScheduleClear
)

// dbxAgents are the agents "Apply to agents" can repoint.
var dbxAgents = []string{"claude", "codex", "opencode", "copilot"}

// dbxState is one env's authType and its no-network expiry text ("expires in 52m", "no login").
func dbxState(reg *providers.Registry, env string) (authType, text string, ok bool) {
	if reg == nil {
		return "", "no login", false
	}
	p := reg.Providers[env]
	if p == nil {
		return "", "no login", false
	}
	authType = "command"
	if p.Auth.Sealed() {
		authType = p.Auth.Type
	}
	if !providers.CredentialPresent(p) {
		return authType, "no login", false
	}
	exp, has := providers.TokenExpiry(env)
	switch {
	case !has:
		return authType, "logged in", true
	case time.Until(exp) <= 0:
		return authType, "expired", true
	}
	return authType, "expires in " + dbxDur(time.Until(exp)), true
}

// dbxIdentityBadge is the Identity row's check column: the set mode and who signs in now ("[auto → sp]"),
// green while that credential's token is live.
func dbxIdentityBadge(reg *providers.Registry, env string) string {
	p := reg.Providers[env]
	if p == nil {
		return ""
	}
	mode, _ := providers.ResolveIdentity(p.Identity, "")
	authType, text, ok := dbxState(reg, env)
	who := map[string]string{providers.AuthM2M: "sp", providers.AuthOAuth: "user", providers.AuthPAT: "pat"}[authType]
	label := mode
	if who != "" && who != mode {
		label += " → " + who
	}
	return Checked(ok && text != "expired", label)
}

// dbxDur renders a remaining lifetime as Nm, or NhMMm from two hours up.
func dbxDur(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 120 {
		return fmt.Sprintf("%dm", max(m, 1))
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// databricksRow is the settings row: the first env holding a login (else the first configured), its authType and expiry.
func databricksRow(d *dialog) homeRow {
	env, state, check := "", "not configured", "no login"
	for _, e := range dbxProfileNames(d.reg) {
		at, text, ok := dbxState(d.reg, e)
		if at == "" {
			continue
		}
		if env == "" || ok && check == "no login" {
			env, state, check = e, e+" · "+at, text
		}
	}
	return homeRow{Name: "databricks", State: state, Check: check, Run: runDatabricksPage}
}

// runDatabricks is the configure section: the page on a terminal, else the token-free report.
func runDatabricks(d *dialog) bool {
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		runDatabricksPage(d)
		return true
	}
	for _, e := range dbxProfileNames(d.reg) {
		at, text, _ := dbxState(d.reg, e)
		if at == "" {
			at = "-"
		}
		d.printf("%-5s %-12s %s\n", e, at, text)
	}
	return true // a report; a missing login is not a failed section
}

// runDatabricksPage loops the env list until Back.
func runDatabricksPage(d *dialog) {
	dbxShell(d).Run("databricks", func() []uidialog.Row {
		var rows []uidialog.Row
		names := dbxProfileNames(d.reg)
		atW := 0
		for _, e := range names {
			at, _, _ := dbxState(d.reg, e)
			atW = max(atW, len(cmp.Or(at, "-")))
		}
		for _, e := range names {
			at, text, _ := dbxState(d.reg, e)
			rows = append(rows, uidialog.Row{Name: e, Detail: fmt.Sprintf("%-*s  %s", atW, cmp.Or(at, "-"), text), Run: func() bool {
				runDatabricksEnv(d, e)
				return false
			}})
		}
		return rows
	})
}

// runDatabricksEnv is one env's action list.
func runDatabricksEnv(d *dialog, env string) {
	act := func(name, detail string, run func()) uidialog.Row {
		return uidialog.Row{Name: name, Detail: detail, Run: func() bool {
			run()
			d.reload()
			return false
		}}
	}
	dbxShell(d).Run("databricks · "+env, func() []uidialog.Row {
		return []uidialog.Row{
			act("Set up", "sign in, verify a token, apply to agents", func() { dbxSetUp(d, env) }),
			act("Login", "browser sign-in (u2m)", func() { dbxDo(d, env, "login", func(c context.Context) error { return dbxLogin(c, env, d.out) }) }),
			act("Paste PAT", "hidden input", func() { dbxPaste(d, env) }),
			act("Mint now", "fresh token, expiry only", func() { dbxMintNow(d, env, false) }),
			act("Copy token", "clipboard, clears in 30s", func() { dbxMintNow(d, env, true) }),
			act("Apply to agents", "repoint all four or one", func() { dbxApply(d, env) }),
			act("Identity", "auto, user or sp; the paired service principal  "+dbxIdentityBadge(d.reg, env),
				func() { dbxIdentity(d, env) }),
		}
	})
}

// dbxDo runs fn under the interrupt-free page context and reports a failure or "done".
func dbxDo(d *dialog, env, what string, fn func(context.Context) error) {
	if err := fn(context.Background()); err != nil {
		d.printf("%s: %s: %v\n", env, what, err)
		return
	}
	d.changed = true
	d.printf("%s: %s done\n", env, what)
}

func dbxPaste(d *dialog, env string) {
	v, err := dbxSecret("Personal access token: ", secretstore.Preview)
	if err != nil || v == "" {
		return
	}
	status, err := dbxSetPAT(context.Background(), env, v)
	if err != nil {
		d.printf("%s: paste PAT: %v\n", env, err)
		return
	}
	d.changed = true
	d.printf("%s: PAT saved · %s\n", env, status)
}

// dbxMintNow mints; the token is only ever handed to the clipboard, never printed.
func dbxMintNow(d *dialog, env string, copyIt bool) {
	ctx := context.Background()
	res, tok, err := dbxMint(ctx, env)
	if err != nil {
		d.printf("%s: mint: %v\n", env, err)
		return
	}
	exp := "no expiry reported"
	if !res.Expiry.IsZero() {
		exp = "expires in " + dbxDur(time.Until(res.Expiry))
	}
	if !copyIt {
		d.printf("%s · %s · minted · %s\n", env, res.AuthType, exp)
		return
	}
	if err := dbxCopy(ctx, tok); err != nil {
		if errors.Is(err, clipboard.ErrNoClipboard) {
			d.printf("%s: no clipboard here; the agents fetch via the proxy\n", env)
			return
		}
		d.printf("%s: copy: %v\n", env, err)
		return
	}
	if err := dbxSchedule(clipboard.DefaultClearAfter, tok); err != nil {
		d.printf("%s: copied, but the timed clear failed: %v\n", env, err)
		return
	}
	d.printf("%s · copied · clears in %ds\n", env, int(clipboard.DefaultClearAfter/time.Second))
}

// dbxProfiles lists the registry's Databricks profiles.
func dbxProfileNames(reg *providers.Registry) []string {
	if reg == nil {
		return nil
	}
	return providers.DatabricksProfiles(reg)
}

// dbxSetUp signs in with OAuth, verifies a token (discarded) and applies the profile, stopping at the first failure.
func dbxSetUp(d *dialog, env string) {
	if err := dbxLogin(context.Background(), env, d.out); err != nil {
		d.printf("%s: set up stopped at sign-in: %v\n", env, err)
		return
	}
	d.changed = true
	res, _, err := dbxMint(context.Background(), env)
	if err != nil {
		d.printf("%s: set up stopped at token check: %v\n", env, err)
		return
	}
	exp := "no expiry reported"
	if !res.Expiry.IsZero() {
		exp = "expires in " + dbxDur(time.Until(res.Expiry))
	}
	d.printf("%s · %s · token verified · %s\n", env, res.AuthType, exp)
	configure.DiscoverAfterSignin()
	cancelled, err := dbxApplyPins(d, env)
	switch {
	case err != nil:
		d.printf("%s: set up stopped at apply: %v\n", env, err)
	case cancelled:
		d.printf("%s: signed in; agents were not changed\n", env)
	}
}

// dbxApply sets one agent's record profile (or all of them) to the profile.
func dbxApply(d *dialog, env string) {
	if _, err := dbxApplyPins(d, env); err != nil {
		d.printf("%s: apply: %v\n", env, err)
	}
}

// dbxApplyPins is dbxApply's engine: cancelled when the picker was dismissed, err when the pins could not be written.
func dbxApplyPins(d *dialog, env string) (cancelled bool, err error) {
	rows := [][2]string{{"All agents", "pin every agent to " + env}}
	for _, a := range dbxAgents {
		rows = append(rows, [2]string{a, "this agent only"})
	}
	idx, perr := dbxShell(d).Pick("apply "+env+" to", rows)
	if perr != nil || idx < 0 {
		return true, nil
	}
	targets := dbxAgents
	if idx > 0 {
		targets = dbxAgents[idx-1 : idx]
	}
	changed, err := configure.ApplyProfile(targets, env)
	if err != nil {
		return false, err
	}
	if len(changed) == 0 {
		d.printf("%s: already applied\n", env)
		return false, nil
	}
	for _, a := range changed {
		d.printf("  %s profile: %s\n", a, env)
	}
	d.changed = true
	return false, nil
}

// dbxIdentity picks env's identity mode, then asks for the SP client id and secret when the mode can use one.
func dbxIdentity(d *dialog, env string) {
	p := d.reg.Providers[env]
	if p == nil {
		return
	}
	modes := []string{providers.IdentityAuto, providers.IdentityUser, providers.IdentitySP}
	cur, _ := providers.ResolveIdentity(p.Identity, "")
	rows := make([][2]string, len(modes))
	for i, m := range modes {
		rows[i] = [2]string{m, uidialog.Mark(m == cur)}
	}
	idx, err := dbxShell(d).Pick("identity · "+env, rows, max(slices.Index(modes, cur), 0))
	if err != nil || idx < 0 {
		return
	}
	spID := p.Auth.SPClientID
	if modes[idx] == providers.IdentityUser { // user mode never reads the SP: keep the stored id and secret
		saveDbxIdentity(d, env, modes[idx], spID, "")
		return
	}
	id, err := dbxSecret("Service principal client id (empty keeps "+cmp.Or(spID, "none")+"): ", func(s string) string { return s })
	if err != nil {
		return
	}
	spID = cmp.Or(id, spID)
	secret := ""
	if spID != "" {
		if secret, err = dbxSecret("Service principal secret (empty keeps the stored one): ", secretstore.Preview); err != nil {
			return
		}
	}
	saveDbxIdentity(d, env, modes[idx], spID, secret)
}

func saveDbxIdentity(d *dialog, env, mode, spID, secret string) {
	if err := configure.SaveDbxIdentity(d.reg, env, mode, spID, secret); err != nil {
		d.printf("%s: identity: %v\n", env, err)
		return
	}
	d.changed = true
	d.printf("%s: identity %s\n", env, mode)
}

// reload refreshes the registry after a login, PAT or apply changed it.
func (d *dialog) reload() {
	if reg, err := providers.Load(); err == nil {
		d.reg = reg
	}
}
