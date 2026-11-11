package menu

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

var (
	dbxLogin = databricks.LoginRow

	dbxMint = databricks.Mint

	dbxSetPAT = databricks.SetPAT

	dbxShell = func(d *dialog) *shell { return d.shell() }

	dbxSecret = tui.SecretHint

	dbxCheckSP = func(ctx context.Context, host, id, secret string) error {
		_, err := oauth.MintM2M(ctx, host, id, secret)
		return err
	}

	dbxCopy = clipboard.WriteSecret

	dbxSchedule = clipboard.ScheduleClear
)

const (
	dbxLoginTimeout = 5 * time.Minute // browser sign-in waits on the user
	dbxNetTimeout   = 30 * time.Second
)

// dbxAgents are the agents "Apply to agents" can repoint.
var dbxAgents = []string{agentid.Claude, agentid.Codex, agentid.OpenCode, agentid.Copilot}

// dbxState is one env's authType (m2m when its service principal minted last) and its no-network expiry text.
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
	st := allSigninFor(p).Status()
	exp := st.Expires
	if e, method, ok := providers.TokenExpiryMethod(p.Name); ok { // the latest mint, the user's or the service principal's
		exp = e
		if method == providers.AuthM2M && authType == providers.AuthOAuth {
			authType = providers.AuthM2M
		}
	}
	switch {
	case !st.Present:
		return authType, "no login", false
	case exp.IsZero():
		return authType, "logged in", true
	case time.Until(exp) <= 0:
		return authType, dbxRenews, true
	}
	return authType, "expires in " + dbxDur(time.Until(exp)), true
}

// dbxRenews is the state of a stored credential whose last token ran out: the next call mints a new one.
const dbxRenews = "renews on use"

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
	return checked(ok && text != dbxRenews, label)
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
	if d.interactive() {
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
	dbxShell(d).Run("databricks", func() []shellRow {
		var rows []shellRow
		names := dbxProfileNames(d.reg)
		ats, texts := make([]string, len(names)), make([]string, len(names))
		atW := 0
		for i, e := range names {
			at, text, _ := dbxState(d.reg, e)
			ats[i], texts[i] = cmp.Or(at, "-"), text
			atW = max(atW, len(ats[i]))
		}
		for i, e := range names {
			detail := fmt.Sprintf("%-*s  %s", atW, ats[i], texts[i])
			rows = append(rows, shellRow{Name: e, Detail: detail, Run: func() bool {
				runDatabricksEnv(d, e)
				return false
			}})
		}
		return rows
	})
}

// runDatabricksEnv is one env's action list.
func runDatabricksEnv(d *dialog, env string) {
	act := func(name, detail string, run func()) shellRow {
		return shellRow{Name: name, Detail: detail, Run: func() bool {
			run()
			d.reload()
			return false
		}}
	}
	dbxShell(d).Run("databricks · "+env, func() []shellRow {
		return []shellRow{
			act("Set up", "sign in, verify a token, apply to agents", func() { dbxSetUp(d, env) }),
			act("Login", "browser sign-in; sets the profile's auth type to oauth", func() { dbxDo(d, env, "login", func(c context.Context) error { return dbxLogin(c, env, d.out) }) }),
			act("Paste PAT", "seal a PAT, probe it; sets the profile's auth type to pat", func() { dbxPaste(d, env) }),
			act("Mint now", "mint a token, never shown (PAT profile: stored PAT)", func() { dbxMintNow(d, env, false) }),
			act("Copy token", "clipboard copy, clears in 30s; PAT copies the stored PAT", func() { dbxMintNow(d, env, true) }),
			act("Apply to agents", "repoint "+strings.Join(dbxAgents, ", ")+": all or one", func() { dbxApply(d, env) }),
			act("Identity", "auto, user or sp; paired service principal "+dbxIdentityBadge(d.reg, env),
				func() { dbxIdentity(d, env) }),
		}
	})
}

// dbxDo runs fn under a login-timeout context and reports a failure or "done".
func dbxDo(d *dialog, env, what string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbxLoginTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		d.printf("%s: %s: %v\n", env, what, err)
		return
	}
	d.changed = true
	d.printf("%s: %s done\n", env, what)
}

func dbxPaste(d *dialog, env string) {
	v, err := dbxSecret("", "Personal access token: ", vault.Preview)
	if err != nil {
		d.printf("%s: paste PAT: %v\n", env, err)
		return
	}
	if v == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbxNetTimeout)
	defer cancel()
	status, err := dbxSetPAT(ctx, env, v)
	if err != nil {
		d.printf("%s: paste PAT: %v\n", env, err)
		return
	}
	d.changed = true
	d.printf("%s: PAT saved · %s\n", env, status)
}

// dbxMintNow mints; the token is only ever handed to the clipboard, never printed.
func dbxMintNow(d *dialog, env string, copyIt bool) {
	ctx, cancel := context.WithTimeout(context.Background(), dbxNetTimeout)
	defer cancel()
	res, tok, err := dbxMint(ctx, env)
	if err != nil {
		d.printf("%s: mint: %v\n", env, err)
		return
	}
	exp := dbxExpiryText(res.Expiry)
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

// dbxProfileNames lists the registry's Databricks profiles.
func dbxProfileNames(reg *providers.Registry) []string {
	if reg == nil {
		return nil
	}
	return providers.DatabricksProfiles(reg)
}

// dbxSetUp signs in with OAuth, verifies a token (discarded) and applies the profile, stopping at the first failure.
func dbxSetUp(d *dialog, env string) {
	loginCtx, cancelLogin := context.WithTimeout(context.Background(), dbxLoginTimeout)
	defer cancelLogin()
	if err := dbxLogin(loginCtx, env, d.out); err != nil {
		d.printf("%s: set up stopped at sign-in: %v\n", env, err)
		return
	}
	d.changed = true
	mintCtx, cancelMint := context.WithTimeout(context.Background(), dbxNetTimeout)
	defer cancelMint()
	res, _, err := dbxMint(mintCtx, env)
	if err != nil {
		d.printf("%s: set up stopped at token check: %v\n", env, err)
		return
	}
	exp := dbxExpiryText(res.Expiry)
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
	rows := [][2]string{{"All agents", "pin " + strings.Join(dbxAgents, ", ") + " to " + env}}
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

// dbxIdentity picks env's identity mode; a service principal already stored (id and secret) is kept unless the
// "change service principal" row is picked, otherwise it asks for the id and secret and checks them before saving.
func dbxIdentity(d *dialog, env string) {
	p := d.reg.Providers[env]
	if p == nil {
		return
	}
	name := providers.SecretName(providers.GroupDatabricks, env, providers.AuthM2M)
	stored, hasSecret, err := vault.ReadSecret(name)
	if err != nil {
		d.printf("%s: identity: %v\n", env, err)
		return
	}
	kept := p.Auth.SPClientID != "" && hasSecret
	mode, change, ok := dbxIdentityMode(d, env, p, kept)
	if !ok {
		return
	}
	dbxIdentityApply(d, env, mode, change, kept, p, stored)
}

// dbxIdentityMode shows the mode picker (plus the change-SP row when kept); mode is cur when change, ok false on cancel.
func dbxIdentityMode(d *dialog, env string, p *providers.Provider, kept bool) (mode string, change, ok bool) {
	modes := []string{providers.IdentityAuto, providers.IdentityUser, providers.IdentitySP}
	cur, _ := providers.ResolveIdentity(p.Identity, "")
	rows := make([][2]string, len(modes), len(modes)+1)
	for i, m := range modes {
		rows[i] = [2]string{m, mark(m == cur)}
	}
	if kept {
		rows = append(rows, [2]string{"change service principal", "new client id and secret"})
	}
	idx, err := dbxShell(d).Pick("identity · "+env, rows, max(slices.Index(modes, cur), 0))
	if err != nil || idx < 0 {
		return "", false, false
	}
	if idx == len(modes) {
		return cur, true, true
	}
	return modes[idx], false, true
}

// dbxIdentityApply saves a picked mode: user keeps the stored pair, a kept pair is confirmed, anything else prompts.
func dbxIdentityApply(d *dialog, env, mode string, change, kept bool, p *providers.Provider, stored string) {
	spID := p.Auth.SPClientID
	if mode == providers.IdentityUser && !change { // user mode never reads the SP: keep the stored id and secret
		saveDbxIdentity(d, env, mode, spID, "", "")
		return
	}
	if kept && !change {
		d.printf("SP %s · secret stored — kept\n", dbxShort(spID))
		saveDbxIdentity(d, env, mode, spID, "", "SP and secret kept")
		return
	}
	dbxIdentityPrompt(d, env, mode, p, stored)
}

// dbxIdentityPrompt asks for the SP id and secret (empty keeps the stored ones), checks the pair, then saves it.
func dbxIdentityPrompt(d *dialog, env, mode string, p *providers.Provider, storedSecret string) {
	id, spID, secret, ok := dbxPromptSP(p, storedSecret)
	if !ok {
		return
	}
	if spID != "" && cmp.Or(secret, storedSecret) == "" {
		d.printf("SP %s has no secret; enter one, or keep identity user\n", dbxShort(spID))
		return
	}
	if err := dbxCheckPair(p, spID, cmp.Or(secret, storedSecret)); err != nil {
		d.printf("SP check failed: %v\n", err)
		return
	}
	saveDbxIdentity(d, env, mode, spID, secret, dbxIdentityNote(env, id, secret, storedSecret))
}

// dbxPromptSP asks for the SP client id and, once one is set, its secret; ok is false when a prompt is cancelled.
func dbxPromptSP(p *providers.Provider, storedSecret string) (id, spID, secret string, ok bool) {
	spID = p.Auth.SPClientID
	hint := ""
	if spID != "" {
		hint = "empty keeps " + dbxShort(spID)
	}
	id, err := dbxSecret(hint, "Service principal client id: ", func(s string) string { return s })
	if err != nil {
		return "", "", "", false
	}
	spID = cmp.Or(id, spID)
	if spID == "" {
		return id, "", "", true
	}
	hint = ""
	if storedSecret != "" {
		hint = "empty keeps the stored secret"
	}
	secret, err = dbxSecret(hint, "Service principal secret: ", vault.Preview)
	if err != nil {
		return "", "", "", false
	}
	return id, spID, secret, true
}

// dbxCheckPair verifies an SP id and secret against the host; a missing half skips the check.
func dbxCheckPair(p *providers.Provider, spID, pair string) error {
	if spID == "" || pair == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbxNetTimeout)
	defer cancel()
	return dbxCheckSP(ctx, providers.AuthHost(p), spID, pair)
}

// dbxNoteSPKept is the note when the save changed neither the SP nor the secret; saveDbxIdentity omits the SP then.
const dbxNoteSPKept = "SP and secret kept"

// dbxIdentityNote words what the save did with the stored secret; "" when it wrote a new one with nothing kept.
func dbxIdentityNote(env, id, secret, storedSecret string) string {
	switch {
	case secret != "":
		return "secret saved (" + providers.SecretName(providers.GroupDatabricks, env, providers.AuthM2M) + ")"
	case storedSecret != "" && id != "":
		return "secret kept"
	case storedSecret != "":
		return dbxNoteSPKept
	}
	return ""
}

// dbxShort is an id as first4…last4 (short ids are shown whole).
func dbxShort(id string) string {
	if r := []rune(id); len(r) > 9 {
		return string(r[:4]) + "…" + string(r[len(r)-4:])
	}
	return id
}

func saveDbxIdentity(d *dialog, env, mode, spID, secret, note string) {
	if err := databricks.SaveIdentity(d.reg, env, mode, spID, secret); err != nil {
		d.printf("%s: identity: %v\n", env, err)
		return
	}
	d.changed = true
	line := env + ": identity " + mode
	if spID != "" && note != "" && note != dbxNoteSPKept {
		line += " · SP " + dbxShort(spID)
	}
	if note != "" {
		line += " · " + note
	}
	d.printf("%s\n", line)
}

// reload refreshes the registry after a login, PAT or apply changed it.
func (d *dialog) reload() {
	reg, err := providers.Load()
	if err != nil {
		d.printf("registry reload failed, showing the previous state: %v\n", err)
		return
	}
	d.reg = reg
}

// dbxExpiryText is the expiry phrase for a minted token's expiry.
func dbxExpiryText(t time.Time) string {
	if t.IsZero() {
		return "no expiry reported"
	}
	return "expires in " + dbxDur(time.Until(t))
}
