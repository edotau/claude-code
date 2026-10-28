// databricks_page.go — the databricks home row and page: per-env login, PAT paste, mint, copy and agent repointing.
package configure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
)

// Seams for the databricks page; tests replace them so no network, vault or clipboard is touched.
var (
	dbxLogin    = databricks.LoginRow
	dbxMint     = databricks.Mint
	dbxSetPAT   = databricks.SetPAT
	dbxPick     = tui.PickBack
	dbxSecret   = tui.Secret
	dbxCopy     = clipboard.WriteSecret
	dbxSchedule = clipboard.ScheduleClear
)

// dbxAgents maps each agent "Apply to agents" can repoint to its <AGENT>_PROFILE pin (copilot's is a GitHub login).
var dbxAgents = []struct{ name, pin string }{
	{"claude", providers.PinProfile}, {"codex", providers.PinCodexProfile}, {"opencode", providers.PinOpencodeProfile},
}

// dbxState is one env's authType and its no-network expiry text ("token expires in 52m", "no login").
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
		return authType, "token expired", true
	}
	return authType, "token expires in " + dbxDur(time.Until(exp)), true
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
	return homeRow{"databricks", state, check, runDatabricksPage}
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
	start := 0
	for {
		rows := make([][2]string, len(dbxProfileNames(d.reg)))
		for i, e := range dbxProfileNames(d.reg) {
			at, text, _ := dbxState(d.reg, e)
			if at == "" {
				at = "-"
			}
			rows[i] = [2]string{e, at + "  " + text}
		}
		idx, err := dbxPick("databricks", rows, start)
		if err != nil || idx < 0 {
			return
		}
		start = idx
		runDatabricksEnv(d, dbxProfileNames(d.reg)[idx])
	}
}

// runDatabricksEnv is one env's action list.
func runDatabricksEnv(d *dialog, env string) {
	acts := []struct {
		label, desc string
		run         func()
	}{
		{"Login", "browser sign-in (u2m)", func() { dbxDo(d, env, "login", func(c context.Context) error { return dbxLogin(c, env, d.out) }) }},
		{"Paste PAT", "hidden input", func() { dbxPaste(d, env) }},
		{"Mint now", "fresh token, expiry only", func() { dbxMintNow(d, env, false) }},
		{"Copy token", "clipboard, clears in 30s", func() { dbxMintNow(d, env, true) }},
		{"Apply to agents", "repoint all four or one", func() { dbxApply(d, env) }},
	}
	rows := make([][2]string, len(acts))
	for i, a := range acts {
		rows[i] = [2]string{a.label, a.desc}
	}
	start := 0
	for {
		idx, err := dbxPick("databricks · "+env, rows, start)
		if err != nil || idx < 0 {
			return
		}
		start = idx
		acts[idx].run()
		d.reload()
	}
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
	v, err := dbxSecret("Personal access token: ", nil)
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

// dbxApply pins one agent's <AGENT>_PROFILE (or all of them) to the profile.
func dbxApply(d *dialog, env string) {
	rows := [][2]string{{"All agents", "pin every agent to " + env}}
	for _, a := range dbxAgents {
		rows = append(rows, [2]string{a.name, "this agent only"})
	}
	idx, err := dbxPick("apply "+env+" to", rows, 0)
	if err != nil || idx < 0 {
		return
	}
	pins := map[string]string{}
	for i, a := range dbxAgents {
		if idx == 0 || idx == i+1 {
			pins[a.pin] = env
		}
	}
	diff, err := providers.PinDiff(pins)
	if err != nil {
		d.printf("%s: apply: %v\n", env, err)
		return
	}
	if len(diff) == 0 {
		d.printf("%s: already applied\n", env)
		return
	}
	if err := providers.SetPins(pins); err != nil {
		d.printf("%s: apply: %v\n", env, err)
		return
	}
	d.changed = true
	for _, l := range diff {
		d.println(l)
	}
}

// reload refreshes the registry after a login, PAT or apply changed it.
func (d *dialog) reload() {
	if reg, err := providers.Load(); err == nil {
		d.reg = reg
	}
}
