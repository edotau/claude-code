package menu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/vault"
)

// Seams for the page's tailscale side effects; tests swap them.
var (
	tsShell    = func(d *dialog) *shell { return d.shell() }
	tsLogin    = tailscale.Login
	tsMint     = tailscale.MintKey
	tsExitNode = tailscale.ExitNode
)

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

// runTailscalePage loops status, sign in (while not connected), admin key, auth key and exit node until Back.
func runTailscalePage(d *dialog) {
	s := tsShell(d)
	act := func(name, detail string, run func()) shellRow {
		return shellRow{Name: name, Detail: detail, Run: func() bool { run(); return false }}
	}
	s.Run("tailscale", func() []shellRow {
		st, err := tailscaleStatus(context.Background())
		state := "not connected"
		if err == nil {
			host, _, _ := strings.Cut(st.DNSName, ".")
			state = "running · " + host
		}
		rows := []shellRow{act("status", state, func() { runRemote(d) })}
		if err != nil {
			rows = append(rows, act("sign in", "run tailscale login; prints the sign-in URL", func() { tsSignIn(d) }))
		}
		admin := map[bool]string{true: "present", false: "absent"}[configure.HasAdminCredential()]
		return append(rows,
			act("admin key", "seal the OAuth client id and secret · "+admin, func() { tsAdminKey(d) }),
			act("auth key", "mint TAILSCALE_AUTHKEY (90d), show once, seal in vault", func() { tsAuthKey(d, s) }),
			act("exit node", tsExitState(st, err)+" · on, off or approve", func() { tsExitNodeMenu(d, s) }))
	})
}

// tsExitState is this box's exit-node state from Status: advertised and approved, or not.
func tsExitState(st tailscale.Local, err error) string {
	if err != nil {
		return "unknown"
	}
	adv := map[bool]string{true: "advertised", false: "not advertised"}[st.ExitNodeOption]
	return adv + " · " + map[bool]string{true: "approved", false: "not approved"}[st.ExitNodeApproved]
}

// forgetTailscaleStatus drops the cached Status so the next read sees a sign-in or exit-node change.
func forgetTailscaleStatus() {
	tsStatus.Lock()
	tsStatus.at = time.Time{}
	tsStatus.Unlock()
}

// tsSignIn runs `tailscale login` on the terminal, then reports the machine name status now shows.
func tsSignIn(d *dialog) {
	if err := tsLogin(context.Background(), d.o.In, d.out, d.out); err != nil {
		d.printf("sign in: %v\n", err)
		return
	}
	forgetTailscaleStatus()
	st, err := tailscaleStatus(context.Background())
	if err != nil {
		d.printf("sign in: login finished, but status reports %v\n", err)
		return
	}
	d.printf("tailscale: signed in as %s\n", strings.TrimSuffix(st.DNSName, "."))
}

// tsAdminKey reads the OAuth client id and secret, refuses a half pair, and stores both like the secrets row.
func tsAdminKey(d *dialog) {
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
	for _, c := range creds {
		if !storeValue(d, c.Name, c.Value) {
			return
		}
	}
}

// tsAuthKey asks the key's options, mints it, prints it once and seals it as TAILSCALE_AUTHKEY.
func tsAuthKey(d *dialog, s *shell) {
	if !configure.HasAdminCredential() {
		d.println("auth key: needs an admin credential; set one with the admin key row")
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

// tsYes asks a [y/N] question on s: only an answer starting with y is yes.
func tsYes(s *shell, question string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Line(question+" [y/N] "))), "y")
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
