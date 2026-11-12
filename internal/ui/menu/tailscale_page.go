package menu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// Seams for the page's tailscale side effects; tests swap them.
var (
	tsShell    = func(d *dialog) *shell { return d.shell() }
	tsLogin    = tailscale.Login
	tsMint     = tailscale.MintKey
	tsExitNode = tailscale.ExitNode
	tsValidate = tailscale.ValidateCreds
	tsCheck    = tailscale.CheckAdmin
)

// tsAdminVerdict caches the admin credential check for a minute; tsAdminKey clears it after a change.
var tsAdminVerdict struct {
	at   time.Time
	text string
}

// tsNetTimeout bounds one admin-API or `tailscale set` action.
const tsNetTimeout = 30 * time.Second

// runTailscale is the tailscale row: the page on a terminal, else runRemote's read-only report.
func runTailscale(d *dialog) {
	if !d.interactive() {
		runRemote(d)
		return
	}
	runTailscalePage(d)
}

// runTailscalePage loops login, status, admin key, auth key and exit node until Back.
func runTailscalePage(d *dialog) {
	s := tsShell(d)
	act := func(name, detail string, run func()) shellRow {
		return shellRow{Name: name, Detail: detail, Run: func() bool { run(); return false }}
	}
	s.Run("tailscale", func() []shellRow {
		st, err := tailscaleStatus(context.Background())
		state := ansi.SpanOff + "not connected" + ansi.SpanEnd
		if err == nil {
			host, _, _ := strings.Cut(st.DNSName, ".")
			state = host + " · " + tsGreen("running")
		}
		login := "browser sign-in"
		if err == nil {
			login = "add or switch account"
		}
		rows := []shellRow{act("login", login, func() { tsSignIn(d) }), act("status", state, func() { runRemote(d) })}
		auth := "mint, show once"
		if _, had, _ := vault.ReadSecret("TAILSCALE_AUTHKEY"); had {
			auth += " · " + tsGreen("vault")
		}
		return append(rows,
			act("admin key", "OAuth client · "+tsAdminState(), func() { tsAdminKey(d, s) }),
			act("auth key", auth, func() { tsAuthKey(d, s) }),
			act("exit node", "advertise this box · "+tsExitState(st, err), func() { tsExitNodeMenu(d, s) }))
	})
}

// tsExitState is this box's current exit-node option in green: on (unapproved when not yet approved) or off.
func tsExitState(st tailscale.Local, err error) string {
	switch {
	case err != nil:
		return ansi.SpanOff + "unknown" + ansi.SpanEnd
	case !st.ExitNodeOption:
		return tsGreen("off")
	case !st.ExitNodeApproved:
		return tsGreen("on") + " · unapproved"
	}
	return tsGreen("on")
}

// tsAdminState is absent, valid (green) or invalid (yellow): the stored credential tried against the admin API.
func tsAdminState() string {
	if !configure.HasAdminCredential() {
		return ansi.SpanOff + "absent" + ansi.SpanEnd
	}
	if time.Since(tsAdminVerdict.at) > time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := tsCheck(ctx)
		var netErr net.Error
		if err != nil && (ctx.Err() != nil || errors.As(err, &netErr)) {
			// a network failure says nothing about the credential: keep any prior verdict and retry in a minute
			if tsAdminVerdict.text == "" {
				tsAdminVerdict.text = ansi.SpanOff + "unknown" + ansi.SpanEnd
			}
		} else if err != nil {
			tsAdminVerdict.text = ansi.SpanWarn + "invalid" + ansi.SpanEnd
		} else {
			tsAdminVerdict.text = tsGreen("valid")
		}
		tsAdminVerdict.at = time.Now()
	}
	return tsAdminVerdict.text
}

func tsGreen(s string) string { return ansi.SpanOK + s + ansi.SpanEnd }

// forgetTailscaleStatus drops the cached Status so the next read sees a sign-in or exit-node change.
func forgetTailscaleStatus() {
	tsStatus.Lock()
	tsStatus.at = time.Time{}
	tsStatus.Unlock()
}

// tsSignIn runs `tailscale login` on the terminal, then reports the machine name status now shows.
func tsSignIn(d *dialog) {
	if err := tsLogin(context.Background(), d.o.In, d.out, d.out); err != nil {
		d.printf("login: %v\n", err)
		return
	}
	forgetTailscaleStatus()
	st, err := tailscaleStatus(context.Background())
	if err != nil {
		d.printf("login: finished, but status reports %v\n", err)
		return
	}
	d.printf("tailscale: signed in as %s\n", strings.TrimSuffix(st.DNSName, "."))
}

// tsAdminKey reads the OAuth client id and secret, refuses a half pair, and seals both in the vault at once.
func tsAdminKey(d *dialog, s *shell) {
	var creds []tailscale.Cred
	for _, name := range tailscale.SetNames(false) {
		v, err := secretLine(d, "Paste "+name+" (input hidden): ")
		if err != nil {
			d.printf("admin key: %v\n", err)
			return
		}
		creds = append(creds, tailscale.Cred{Name: name, Value: strings.TrimSpace(v)})
	}
	if err := tailscale.CheckCreds(creds); err != nil {
		d.printf("admin key: %v; nothing changed\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tsNetTimeout)
	defer cancel()
	if err := tsValidate(ctx, creds[0].Value, creds[1].Value); err != nil {
		d.printf("admin key: Tailscale rejected it (%v); nothing changed\n", err)
		return
	}
	for _, c := range creds {
		if !tsReplaceOK(d, s, c.Name, c.Value) {
			return
		}
	}
	if err := tailscale.StoreCreds(creds); err != nil {
		d.printf("admin key: %v; nothing changed\n", err)
		return
	}
	d.println("admin key: sealed in the vault")
	tsAdminVerdict.at = time.Time{}
}

// tsAuthKey asks the key's options, mints it, prints it once and seals it as TAILSCALE_AUTHKEY.
func tsAuthKey(d *dialog, s *shell) {
	if !configure.HasAdminCredential() {
		d.println("auth key: needs an admin credential; set one with the admin key row")
		return
	}
	if !tsReplaceOK(d, s, "TAILSCALE_AUTHKEY", "") {
		return
	}
	o := tailscale.KeyOptions{Expiry: "90d", Reusable: tsYes(s, "reusable?"), Ephemeral: tsYes(s, "ephemeral?"),
		Tags: strings.TrimSpace(s.Line("tags (tag:a,tag:b; blank = none): "))}
	ctx, cancel := context.WithTimeout(context.Background(), tsNetTimeout)
	defer cancel()
	key, err := tsMint(ctx, o)
	if err != nil {
		d.printf("auth key: %v\n", err)
		return
	}
	fmt.Fprintln(d.out, key.Key) // unpainted: the one place the key is shown
	if err := tailscale.SaveAuthKey(key.Key); err != nil {
		d.printf("auth key: not saved: %v\n", err)
		return
	}
	d.printf("auth key: saved TAILSCALE_AUTHKEY to the vault (%s)\n", vault.Redact(key.Key))
}

// tsReplaceOK asks before overwriting a different stored value of name; "no" (the default) changes nothing.
func tsReplaceOK(d *dialog, s *shell, name, next string) bool {
	old, had, err := vault.ReadSecret(name)
	if err != nil {
		d.printf("%s: %v; nothing changed\n", name, err)
		return false
	}
	if !had || (next != "" && old == next) {
		return true
	}
	if tsYes(s, "replace the stored "+name+"?") {
		return true
	}
	d.printf("%s kept; nothing changed\n", name)
	return false
}

// tsYes asks a [y/N] question on s: only an answer starting with y is yes.
func tsYes(s *shell, question string) bool {
	return tui.ParseYesNo(s.Line(question+" [y/N] "), false)
}

// tsExitNodeMenu picks on, off or (with an admin credential) approve and runs it.
func tsExitNodeMenu(d *dialog, s *shell) {
	modes := []string{"on", "off"}
	rows := [][2]string{{"on", "advertise this box as an exit node"}, {"off", "stop advertising it"}}
	if configure.HasAdminCredential() {
		modes = append(modes, "approve")
		rows = append(rows, [2]string{"approve", "approve its exit-node routes via the admin API"})
	}
	idx, err := s.Pick("exit node", rows)
	if err != nil || idx < 0 || idx >= len(modes) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tsNetTimeout)
	defer cancel()
	msg, err := tsExitNode(ctx, modes[idx])
	forgetTailscaleStatus()
	if err != nil {
		d.printf("exit node %s: %v\n", modes[idx], err)
		return
	}
	d.println(msg)
}
