package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/platform/vault"
)

// errNoCLI: the `tailscale` binary is not on PATH.
var errNoCLI = errors.New("tailscale: `tailscale` CLI not found on PATH")

// errNotRunning: the CLI is on PATH but its backend is not Running (Local is still returned).
var errNotRunning = errors.New("tailscale: backend not Running (see `tailscale status`)")

// Seams for tests: a PATH-stubbed fake `tailscale` or a stubbed exec.CommandContext.
var (
	lookPathReal       = paths.LookPathReal
	execCommandContext = exec.CommandContext
	goos               = runtime.GOOS
	macAppCLI          = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
)

// StubMacAppCLI points the macOS GUI-app fallback at path ("" = none), for tests in other packages; restore undoes it.
func StubMacAppCLI(path string) (restore func()) {
	orig := macAppCLI
	macAppCLI = path
	return func() { macAppCLI = orig }
}

// defaultExecTimeout bounds every quick exec of the installed CLI; connectTimeout bounds `tailscale up`.
const (
	defaultExecTimeout = 5 * time.Second
	connectTimeout     = 60 * time.Second
)

// peer is one other tailnet node from `tailscale status --json`.
type peer struct {
	DNSName string
	IPs     []string
	OS      string
	Online  bool
	// ExitNode is true while this peer is the exit node in use; ExitNodeOption while it advertises one.
	ExitNode       bool
	ExitNodeOption bool
	LastSeen       time.Time
}

// Local is this node's status.
type Local struct {
	Version      string // the client build, e.g. 1.104.7-t…-g…
	ID           string // the stable node id, which the admin API accepts as a device id
	BackendState string
	DNSName      string
	IPs          []string
	// ExitNodeOption is true once this node is allowed to act as an exit node.
	ExitNodeOption bool
	// ExitNodeApproved is true once Self.AllowedIPs carries both default routes.
	ExitNodeApproved bool
	// SubnetRoutes are the approved subnet routes in Self.AllowedIPs (own addresses and defaults excluded).
	SubnetRoutes []string
	Peers        []peer
	LoginName    string `json:"-"` // the signed-in user, empty when status names none
	NodeKey      string `json:"-"` // this node's public key; a browser re-auth rotates it
	Via          string `json:"-"` // "localapi" or "cli": which path answered
}

// rawStatus mirrors the subset of `tailscale status --json` this package reads.
type rawStatus struct {
	Version      string
	BackendState string
	Self         rawNode
	Peer         map[string]rawNode
	User         map[string]struct{ LoginName string }
}

type rawNode struct {
	ID             string
	PublicKey      string
	UserID         int64
	DNSName        string
	TailscaleIPs   []string
	OS             string
	AllowedIPs     []string
	Online         bool
	ExitNode       bool
	ExitNodeOption bool
	LastSeen       time.Time
}

// defaultRoutes are the two routes an exit node advertises.
var defaultRoutes = []string{"0.0.0.0/0", "::/0"}

// hasDefaultRoutes is true once allowed carries both default routes.
func hasDefaultRoutes(allowed []string) bool {
	return !slices.ContainsFunc(defaultRoutes, func(r string) bool { return !slices.Contains(allowed, r) })
}

// subnetRoutes is routes minus the default routes: what a subnet router advertises or has enabled.
func subnetRoutes(routes []string) []string {
	return slices.DeleteFunc(slices.Clone(routes), func(r string) bool { return slices.Contains(defaultRoutes, r) })
}

// subnetRoutesOf is Self.AllowedIPs minus the node's own /32 and /128 and the two default routes.
func subnetRoutesOf(self rawNode) []string {
	own := make([]string, 0, len(self.TailscaleIPs))
	for _, ip := range self.TailscaleIPs {
		suffix := "/32"
		if strings.Contains(ip, ":") {
			suffix = "/128"
		}
		own = append(own, ip+suffix)
	}
	var out []string
	for _, r := range subnetRoutes(self.AllowedIPs) {
		if !slices.Contains(own, r) {
			out = append(out, r)
		}
	}
	return out
}

// advertiseRoutes sets this box's subnet routes (LocalAPI prefs, else `set --advertise-routes`); empty withdraws.
func advertiseRoutes(ctx context.Context, routes []string) error {
	cur, err := currentRoutes(ctx)
	if err == nil {
		next := slices.Clone(routes)
		if hasDefaultRoutes(cur) {
			next = append(next, defaultRoutes...)
		}
		err = patchPrefs(ctx, prefsMask{AdvertiseRoutes: next, AdvertiseRoutesSet: true})
	}
	if err == nil || !errors.Is(err, errNoLocalAPI) {
		return err
	}
	return set(ctx, "--advertise-routes="+strings.Join(routes, ","))
}

// set runs `tailscale set <flags>` with the CLI's output passed through.
func set(ctx context.Context, flags ...string) error {
	bin, err := tailscaleBin()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, bin, cliArgs(append([]string{"set"}, flags...)...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return execErr(ctx, "set", cmd.Run())
}

// cliArgs adds --socket when only the harness's rootless tailscaled (install --tools) is listening.
func cliArgs(args ...string) []string {
	if _, err := os.Stat("/var/run/tailscale/tailscaled.sock"); err == nil {
		return args
	}
	if _, err := os.Stat(harnessSocket()); err != nil {
		return args
	}
	return append([]string{"--socket=" + harnessSocket()}, args...)
}

// execErr wraps a failed CLI run with its verb, a timeout note and the CLI's captured stderr; nil stays nil.
func execErr(ctx context.Context, verb string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out: %w", err)
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return fmt.Errorf("tailscale %s: %w", verb, err)
}

// tailscaleBin resolves the installed CLI: PATH, then on macOS the GUI app's bundled binary; errNoCLI otherwise.
func tailscaleBin() (string, error) {
	if bin, err := lookPathReal("tailscale"); err == nil {
		return bin, nil
	}
	fi, err := os.Stat(macAppCLI)
	if goos == "darwin" && err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
		return macAppCLI, nil
	}
	return "", errNoCLI
}

// Status reads the LocalAPI, else `tailscale status --json`; errNotRunning keeps it.
func Status(ctx context.Context) (Local, error) {
	out, err := localAPI(ctx, http.MethodGet, "status", nil)
	via := "localapi"
	if err != nil {
		if !errors.Is(err, errNoLocalAPI) {
			return Local{}, err
		}
		out, err = statusCLI(ctx)
		via = "cli"
	}
	if err != nil {
		return Local{}, err
	}
	return parseStatus(out, via)
}

// statusCLI is `tailscale status --json`'s raw output; errNoCLI without the CLI.
func statusCLI(ctx context.Context) ([]byte, error) {
	bin, err := tailscaleBin()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, bin, cliArgs("status", "--json")...)
	cmd.WaitDelay = proc.ExecWaitDelay // a wrapper's child holding the pipes must not outlive the timeout
	out, err := proc.OutputCapped(cmd)
	if err != nil {
		return nil, execErr(ctx, "status --json", err)
	}
	return out, nil
}

// parseStatus decodes status JSON from either path; errNotRunning (Local still set) when stopped.
func parseStatus(out []byte, via string) (Local, error) {
	var raw rawStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		return Local{}, fmt.Errorf("tailscale status: %w", err)
	}
	local := Local{
		Version:          raw.Version,
		ID:               raw.Self.ID,
		NodeKey:          raw.Self.PublicKey,
		LoginName:        raw.User[strconv.FormatInt(raw.Self.UserID, 10)].LoginName,
		BackendState:     raw.BackendState,
		DNSName:          raw.Self.DNSName,
		IPs:              raw.Self.TailscaleIPs,
		ExitNodeOption:   raw.Self.ExitNodeOption,
		ExitNodeApproved: hasDefaultRoutes(raw.Self.AllowedIPs),
		SubnetRoutes:     subnetRoutesOf(raw.Self),
		Via:              via,
	}
	if len(raw.Peer) > 0 { // nil stays nil when there are no peers, so JSON keeps null
		local.Peers = make([]peer, 0, len(raw.Peer))
	}
	for _, p := range raw.Peer {
		local.Peers = append(local.Peers, peer{
			DNSName: p.DNSName, IPs: p.TailscaleIPs, OS: p.OS, Online: p.Online,
			ExitNode: p.ExitNode, ExitNodeOption: p.ExitNodeOption, LastSeen: p.LastSeen,
		})
	}
	slices.SortFunc(local.Peers, func(a, b peer) int { return strings.Compare(a.DNSName, b.DNSName) })
	if raw.BackendState != "Running" {
		return local, errNotRunning
	}
	return local, nil
}

// connect runs `tailscale up --auth-key=file:<0600 temp>` (never argv, which `ps` shows) unless already up.
func connect(ctx context.Context, authKey string, exitNode bool) error {
	if st, ok := connected(ctx); ok && (!exitNode || st.ExitNodeApproved) {
		return nil
	}
	lctx, lcancel := context.WithTimeout(ctx, connectTimeout)
	err := loginLocal(lctx, func(c context.Context) error { return startWithKey(c, authKey, exitNode) }, nil)
	lcancel()
	if !errors.Is(err, errNoLocalAPI) {
		return err // past the first request the key may be spent; never re-send it through the CLI
	}
	bin, err := tailscaleBin()
	if err != nil {
		return err
	}
	keyFile, err := writeAuthKeyFile(authKey)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(keyFile) }() // best-effort cleanup of the auth-key temp file
	args := []string{"up", "--auth-key=file:" + keyFile}
	if exitNode {
		args = append(args, "--advertise-exit-node")
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, bin, cliArgs(args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return execErr(ctx, "up", cmd.Run())
}

// loginTimeout bounds `tailscale login`: the browser sign-in waits on the user.
var loginTimeout = 10 * time.Minute

// Login runs `tailscale login` on in/out/errOut, so the user sees the sign-in URL, then audits the connect.
func Login(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	if st, err := Status(ctx); err == nil || errors.Is(err, errNotRunning) {
		if st.Via == "localapi" && !st.running() {
			lctx, lcancel := context.WithTimeout(ctx, loginTimeout)
			err := loginLocal(lctx, func(c context.Context) error { return startWithKey(c, "", false) }, out)
			lcancel()
			if err == nil {
				auditConnect(ctx)
				return nil
			}
			if !errors.Is(err, errNoLocalAPI) {
				return err
			}
		}
	}
	bin, err := tailscaleBin()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, bin, cliArgs("login")...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	if err := execErr(ctx, "login", cmd.Run()); err != nil {
		return err
	}
	auditConnect(ctx)
	return nil
}

// writeAuthKeyFile seals authKey into a 0600 temp file under the harness state dir for `--auth-key=file:`.
func writeAuthKeyFile(authKey string) (string, error) {
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "tailscale-authkey-*")
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name()) // best-effort cleanup; the Chmod error is already being returned
		return "", err
	}
	_, err = f.WriteString(authKey)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name()) // best-effort cleanup; the write/close error is already being returned
		return "", err
	}
	return f.Name(), nil
}

// advertiseExitNode toggles exit-node advertisement (LocalAPI prefs, else `set --advertise-exit-node[=false]`).
func advertiseExitNode(ctx context.Context, on bool) error {
	cur, err := currentRoutes(ctx)
	if err == nil {
		next := subnetRoutes(cur)
		if on {
			next = append(next, defaultRoutes...)
		}
		err = patchPrefs(ctx, prefsMask{AdvertiseRoutes: next, AdvertiseRoutesSet: true})
	}
	if err == nil || !errors.Is(err, errNoLocalAPI) {
		return err
	}
	flag := "--advertise-exit-node"
	if !on {
		flag += "=false"
	}
	return set(ctx, flag)
}

// useExitNode routes this box's traffic through a peer exit node (LocalAPI prefs, else `set --exit-node=<peer>`).
func useExitNode(ctx context.Context, peer string) error {
	ip, ok, err := exitNodeIP(ctx, peer)
	if err != nil {
		return err
	}
	if ok {
		m := prefsMask{ExitNodeIP: ip, ExitNodeIPSet: true, ExitNodeIDSet: true,
			ExitNodeAllowLANAccess: peer != "", ExitNodeAllowLANAccessSet: peer != ""}
		if err := patchPrefs(ctx, m); !errors.Is(err, errNoLocalAPI) {
			return err
		}
	}
	flags := []string{"--exit-node=" + peer}
	if peer != "" {
		flags = append(flags, "--exit-node-allow-lan-access=true")
	}
	return set(ctx, flags...)
}

// exitNodeIP is peer's first tailnet IP ("" clears) from LocalAPI status; ok false leaves resolving to the CLI.
func exitNodeIP(ctx context.Context, peer string) (string, bool, error) {
	if peer == "" {
		return "", true, nil
	}
	if ip, err := netip.ParseAddr(peer); err == nil {
		return ip.String(), true, nil
	}
	out, err := localAPI(ctx, http.MethodGet, "status", nil)
	if err != nil {
		if errors.Is(err, errNoLocalAPI) {
			return "", false, nil
		}
		return "", false, err
	}
	st, err := parseStatus(out, "localapi")
	if err != nil {
		return "", false, err
	}
	for _, p := range st.Peers {
		if _, ok := matchMachine([]string{strings.TrimSuffix(p.DNSName, ".")}, peer); ok && len(p.IPs) > 0 {
			return p.IPs[0], true, nil
		}
	}
	return "", false, nil
}

// running reports whether the backend is Running.
func (l Local) running() bool { return l.BackendState == "Running" }

// Machine is one tailnet node, merged from the admin API and/or the local CLI status.
type Machine struct {
	Name               string
	Addresses          []string
	OS                 string
	Online             bool
	LastSeen           time.Time
	Tags               []string
	ExitNodeAdvertised bool
	ExitNodeApproved   bool
	Source             string // "api" | "cli"
}

// Machines lists tailnet machines and refreshes the machine-name cache ResolveHost and completion read.
func Machines(ctx context.Context) ([]Machine, error) {
	machines, err := liveMachines(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		names = append(names, m.Name)
	}
	WriteMachinesCache(names)
	return machines, nil
}

// liveMachines merges admin-API devices with CLI status (fetched concurrently); no credential means CLI only.
func liveMachines(ctx context.Context) ([]Machine, error) {
	client, err := clientFromVault()
	if errors.Is(err, errNoCredential) {
		return machinesFromStatus(ctx)
	}
	if err != nil {
		return nil, err
	}
	var (
		st         Local
		stErr      error
		statusDone = make(chan struct{})
	)
	go func() {
		defer close(statusDone)
		st, stErr = Status(ctx)
	}()
	devices, err := client.devices(ctx)
	<-statusDone
	if err != nil {
		return nil, err
	}
	// A stopped backend or a missing CLI leaves no live status: the API devices stay offline instead of failing.
	if stErr != nil && !errors.Is(stErr, errNotRunning) && !errors.Is(stErr, errNoCLI) {
		return nil, fmt.Errorf("online state unavailable: %w", stErr)
	}
	byAddr := peersByAddr(st)
	out := make([]Machine, 0, len(devices))
	for _, d := range devices {
		m := Machine{Name: d.Name, Addresses: d.Addresses, OS: d.OS, LastSeen: d.LastSeen, Tags: d.Tags, Source: "api",
			ExitNodeApproved: hasDefaultRoutes(d.EnabledRoutes)}
		for _, a := range d.Addresses {
			if p, ok := byAddr[a]; ok {
				pm := machineFromPeer(p)
				m.Online, m.ExitNodeAdvertised = pm.Online, pm.ExitNodeAdvertised // LastSeen stays the device's
				break
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// machineFromPeer maps a CLI status peer to a Machine; an exit node in use counts as advertised.
func machineFromPeer(p peer) Machine {
	return Machine{Name: p.DNSName, Addresses: p.IPs, OS: p.OS, Online: p.Online, LastSeen: p.LastSeen,
		ExitNodeAdvertised: p.ExitNodeOption || p.ExitNode, Source: "cli"}
}

// peersByAddr indexes the status peers (and this node) by tailnet address.
func peersByAddr(st Local) map[string]peer {
	byAddr := map[string]peer{}
	for _, p := range st.Peers {
		for _, a := range p.IPs {
			byAddr[a] = p
		}
	}
	// This node is never in its own peer list: Running is its online state.
	for _, a := range st.IPs {
		byAddr[a] = peer{Online: st.running(), ExitNodeOption: st.ExitNodeOption}
	}
	return byAddr
}

// machinesFromStatus is the no-credential fallback: self plus every CLI-visible peer.
func machinesFromStatus(ctx context.Context) ([]Machine, error) {
	st, err := CachedStatus(ctx)
	if err != nil && !errors.Is(err, errNotRunning) {
		return nil, err
	}
	out := []Machine{{Name: st.DNSName, Addresses: st.IPs, Online: st.running(),
		ExitNodeAdvertised: st.ExitNodeOption, ExitNodeApproved: st.ExitNodeApproved, Source: "cli"}}
	for _, p := range st.Peers {
		out = append(out, machineFromPeer(p))
	}
	return out, nil
}

// Doctor is one check line: CLI, backend and node, exit-node state, credential name; each step waits at most 4s.
func Doctor(ctx context.Context) (ok bool, detail string) {
	credCtx, cancelCred := context.WithTimeout(ctx, 4*time.Second)
	note, credOK := credentialNote(credCtx)
	cancelCred()
	statusCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	st, err := Status(statusCtx)
	switch {
	case errors.Is(err, errNoCLI):
		return false, "cli not found on PATH" + note
	case errors.Is(err, errNotRunning):
		return false, fmt.Sprintf("backend %s (not connected)%s", st.BackendState, note)
	case err != nil:
		return false, "status: " + err.Error() + note
	}
	exit := "exit-node off"
	switch {
	case st.ExitNodeApproved:
		exit = "exit-node approved"
	case st.ExitNodeOption:
		exit = "exit-node advertised, not approved"
	}
	if st.Via == "cli" {
		exit += " (cli fallback)"
	}
	return credOK, fmt.Sprintf("%s, node %s, %s%s", st.BackendState, st.DNSName, exit, note)
}

// CredState is one credential's presence, by name only — never its value.
type CredState struct {
	Name    string
	Source  string
	Present bool
}

// CredentialNames are the Tailscale credentials the harness stores, in the order Credentials and credentialNote report.
var CredentialNames = []string{oauthIDName, oauthSecretName, apiKeyName, authKeyName}

// Credentials reports presence and source for every Tailscale credential name, never a value.
func Credentials() []CredState {
	out := make([]CredState, len(CredentialNames))
	for i, name := range CredentialNames {
		_, source, ok := vault.Get(name)
		out[i] = CredState{Name: name, Source: source, Present: ok}
	}
	return out
}

// credentialNote names the configured admin credential and, when well-formed, mints a token to confirm it works.
func credentialNote(ctx context.Context) (note string, ok bool) {
	present := map[string]bool{}
	for _, c := range Credentials() {
		present[c.Name] = c.Present
	}
	id, sec, api := present[oauthIDName], present[oauthSecretName], present[apiKeyName]
	switch {
	case id && sec:
		return oauthNote(ctx)
	case id != sec && !api:
		return "; api credential incomplete: need both TAILSCALE_OAUTH_CLIENT_ID and _SECRET", false
	case api:
		return apiKeyNote()
	}
	return "; no admin credential", true
}

// oauthNote validates the OAuth client pair's shape, then mints a token to confirm it works.
func oauthNote(ctx context.Context) (note string, ok bool) {
	idVal, _, _ := vault.Get(oauthIDName)
	secVal, _, _ := vault.Get(oauthSecretName)
	if err := textutil.ValidCredential(oauthIDName, idVal); err != nil {
		return fmt.Sprintf("; TAILSCALE_OAUTH_CLIENT_ID malformed: %v", err), false
	}
	if err := textutil.ValidCredential(oauthSecretName, secVal); err != nil {
		return fmt.Sprintf("; TAILSCALE_OAUTH_CLIENT_SECRET malformed: %v", err), false
	}
	if _, err := newOAuthClient(idVal, secVal).token(ctx); err != nil {
		reason := strings.TrimPrefix(err.Error(), "tailscale: oauth token mint: ")
		return fmt.Sprintf("; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET rejected (%s)", reason), false
	}
	return "; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET verified", true
}

// apiKeyNote validates the API key's shape.
func apiKeyNote() (note string, ok bool) {
	keyVal, _, _ := vault.Get(apiKeyName)
	if err := textutil.ValidCredential(apiKeyName, keyVal); err != nil {
		return fmt.Sprintf("; TAILSCALE_API_KEY malformed: %v", err), false
	}
	return "; api credential TAILSCALE_API_KEY", true
}

// ClientVersion is the installed client's release number ("1.104.7"), from the LocalAPI else the CLI's status.
func ClientVersion(ctx context.Context) (string, error) {
	st, err := Status(ctx)
	if st.Version == "" {
		if err == nil {
			err = errors.New("status carries no version")
		}
		return "", err
	}
	v, _, _ := strings.Cut(st.Version, "-")
	return v, nil
}

// AutoUpdateApply reports the AutoUpdate.Apply pref; known is false when the LocalAPI is unreachable.
func AutoUpdateApply(ctx context.Context) (on, known bool) {
	out, err := localAPI(ctx, http.MethodGet, "prefs", nil)
	if err != nil {
		return false, false
	}
	var p struct {
		AutoUpdate struct{ Apply json.RawMessage }
	}
	if json.Unmarshal(out, &p) != nil || len(p.AutoUpdate.Apply) == 0 {
		return false, false
	}
	return string(p.AutoUpdate.Apply) == "true", true
}
