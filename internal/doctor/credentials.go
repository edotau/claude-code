package doctor

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// credentialProbeWorkers caps the concurrent live credential probes doctor runs.
const credentialProbeWorkers = 4

// probeCredential is the live credential probe; a var so tests can substitute a fake.
var probeCredential = providers.ProbeCredential

var readAudit = audit.Read

// providerCredentialChecks runs providerCredentialCheck for names concurrently and returns the rows in names order.
func providerCredentialChecks(ctx context.Context, reg *providers.Registry, sel providers.Selection, selectErr error, names []string) []check {
	agents := sync.OnceValue(providers.AgentRecords) // one vault read per run, not per profile
	seals := sync.OnceValue(recentSealFailures)      // one audit read per run
	rows, err := parallel.Map(context.Background(), names, credentialProbeWorkers,
		func(_ context.Context, name string) (check, error) {
			return providerCredentialCheck(ctx, reg, sel, selectErr, name, agents, seals), nil
		})
	parallel.Rethrow(err)
	return rows
}

func providerCredentialCheck(ctx context.Context, reg *providers.Registry, sel providers.Selection, selectErr error, name string,
	agents func() map[string]vault.AgentRecord, seals func() []audit.Event) check {
	p := reg.Providers[name]
	session := selectErr == nil && sel.Provider == p
	label := credentialBaseLabel(p, session)
	present := providers.CredentialPresent(p)
	if present {
		present, label = probeCredentialLabel(ctx, p, label)
	}
	loginDead := false
	if p.Auth.Type == providers.AuthOAuth {
		var login string
		login, loginDead = userLoginLabel(p)
		label += login + sealFailureLabel(p, seals())
	}
	if !present {
		present, label = alternativeCredentialLabel(name, label)
	}
	if !present && !session && !profileInUse(reg, name, agents) {
		present, label = true, label+" · unused"
	}
	label += credentialSourceLabel(p)
	// A dead user login is a soft warning (the row still works through the service principal): never a hard failure.
	return check{Name: "credential " + name, OK: present && !loginDead, Hard: session && !loginDead, Detail: label}
}

// credentialBaseLabel is the row's auth method, its env var and the session-profile marker.
func credentialBaseLabel(p *providers.Provider, session bool) string {
	label := p.Auth.Method()
	if p.Auth.Env != "" {
		label += " " + p.Auth.Env
	}
	if session {
		label += " (session profile)"
	}
	return label
}

// credentialSourceLabel names where the credential comes from (names only, never a value); information, not a check.
func credentialSourceLabel(p *providers.Provider) string {
	meta, err := vault.LoadMeta()
	if err != nil {
		return ""
	}
	if p.Auth.Type == providers.AuthOAuth { // u2m reads only the vault, never env
		if _, ok := meta[providers.AuthSecretName(p)]; ok {
			return " · source=vault"
		}
	} else if label := envOrVaultSource(providers.CredentialNames(p), meta); label != "" {
		return label
	}
	if p.Auth.Command != "" {
		return " · source=command"
	}
	return ""
}

func envOrVaultSource(names []string, meta map[string]vault.SecretMeta) string {
	for _, n := range names {
		switch src, shadowed := vault.Locate(n, meta); {
		case src == "env" && shadowed != "":
			return " · source=env " + n + " (shadows the vault entry)"
		case src == "env":
			return " · source=env " + n
		case src == "vault":
			return " · source=vault"
		}
	}
	return ""
}

func alternativeCredentialLabel(name, label string) (bool, string) {
	alt := altAuth(name)
	if alt == "" {
		return false, label
	}
	if !strings.Contains(label, "REJECTED") {
		label += " · no key"
	}
	return true, label + " · " + alt
}

func probeCredentialLabel(ctx context.Context, p *providers.Provider, label string) (bool, string) {
	present := true
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	status, detail := probeCredential(pctx, p)
	cancel()
	switch status {
	case providers.ProbeLogin:
		if p.Auth.Login == "" { // the Claude Max login: say whether unattended runs have a token too
			set, err := headlessTokenState()
			switch {
			case err != nil:
				label += " · headless token unreadable: " + err.Error()
			case set:
				label += " · headless token set"
			default:
				label += " · no headless token (claude-code configure → claude mints one)"
			}
		}
	case providers.ProbeVerified:
		label += " · verified"
	case providers.ProbeRejected:
		present = false                           // stored, but the provider refuses it: as unusable as a missing key
		detail, _, _ = strings.Cut(detail, ": {") // drop the JSON error body the probe appends
		label += " · REJECTED: " + detail
	default:
		label += " · " + status + ": " + detail
	}
	return present, label
}

// userLoginLabel reports an oauth row's user login from what is already recorded, with no network call
func userLoginLabel(p *providers.Provider) (label string, dead bool) {
	if p.Identity == providers.IdentitySP || !providers.CredentialPresent(p) {
		return "", false
	}
	st, dead := providers.LoginStatus(p)
	if !dead {
		if _, known := providers.TokenExpiry(p.Name); known {
			return "", false
		}
		return " · user login unknown", false
	}
	cmd := providers.LoginCommand(p.Name)
	if p.Identity != providers.IdentityUser && providers.SPPresent(p) {
		return fmt.Sprintf(" · warn on the service principal; your own login is not signed in (%s; to sign in: %s)", st.State,
			providers.SignInHint(p.Name)), true
	}
	return fmt.Sprintf(" · warn user login dead (%s): run %s", st.State, cmd), true
}

// recentSealFailures is the last day's seal_failed audit events; an unreadable log reads as none.
func recentSealFailures() []audit.Event {
	ev, _ := readAudit(time.Now().Add(-24 * time.Hour))
	return slices.DeleteFunc(ev, func(e audit.Event) bool { return e.Action != audit.SealFailed })
}

// sealFailureLabel is " · last token seal failed 3h ago (audit: seal_failed)" for p's newest failure, else "".
func sealFailureLabel(p *providers.Provider, seals []audit.Event) string {
	mine := []string{providers.AuthSecretName(p), providers.SecretName(p.Provider, p.Name, providers.AuthM2M) + "_ACCESS"}
	var last time.Time
	for _, e := range seals {
		if slices.Contains(mine, e.Resource) && e.Time.After(last) {
			last = e.Time
		}
	}
	if last.IsZero() {
		return ""
	}
	age := time.Since(last)
	ago := fmt.Sprintf("%dm", int(age/time.Minute))
	if age >= time.Hour {
		ago = fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return " · last token seal failed " + ago + " ago (audit: seal_failed)"
}

// profileInUse reports whether the registry default/fallback chain or any agent's profile pin names the profile.
func profileInUse(reg *providers.Registry, name string, agents func() map[string]vault.AgentRecord) bool {
	if name == reg.Default || slices.Contains(reg.Fallback, name) {
		return true
	}
	for _, rec := range agents() {
		if rec.Profile == name {
			return true
		}
	}
	return false
}

// headlessTokenState reports whether the headless OAuth token is set (env, else vault); err is a vault read failure.
func headlessTokenState() (set bool, err error) {
	if strings.TrimSpace(os.Getenv(providers.OAuthTokenEnv)) != "" {
		return true, nil
	}
	v, _, err := vault.ReadSecret(providers.OAuthTokenEnv)
	return v != "", err
}

// altAuth names the sign-in covering a provider whose key is absent or rejected (claude's OAuth login), or "".
func altAuth(name string) string {
	if name != "anthropic" {
		return ""
	}
	set, err := headlessTokenState()
	if set {
		return "claude uses the OAuth token instead"
	}
	unreadable := ""
	if err != nil {
		unreadable = "; OAuth token unreadable: " + err.Error()
	}
	if st := agent.AuthStatus("claude"); st.Present {
		plan := ""
		if st.PlanHint != "" {
			plan = " (" + st.PlanHint + ")"
		}
		return "claude uses the OAuth login" + plan + " instead" + unreadable
	}
	return ""
}
