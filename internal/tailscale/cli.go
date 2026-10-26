package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// ErrNoCLI: the `tailscale` binary is not on PATH.
var ErrNoCLI = errors.New("tailscale: `tailscale` CLI not found on PATH")

// ErrNotRunning: the CLI is on PATH but its backend is not Running (Local is still returned).
var ErrNotRunning = errors.New("tailscale: backend not Running (see `tailscale status`)")

// Seams for tests: a PATH-stubbed fake `tailscale` or a stubbed exec.CommandContext.
var (
	lookPathReal       = paths.LookPathReal
	execCommandContext = exec.CommandContext
)

// defaultExecTimeout bounds every exec of the installed CLI.
const defaultExecTimeout = 5 * time.Second

// Peer is one other tailnet node from `tailscale status --json`.
type Peer struct {
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
	BackendState string
	DNSName      string
	IPs          []string
	// ExitNodeOption is true once this node is allowed to act as an exit node.
	ExitNodeOption bool
	// ExitNodeApproved is true once Self.AllowedIPs carries both default routes.
	ExitNodeApproved bool
	// SubnetRoutes are the approved subnet routes in Self.AllowedIPs (own addresses and defaults excluded).
	SubnetRoutes []string
	Peers        []Peer
}

// rawStatus mirrors the subset of `tailscale status --json` this package reads.
type rawStatus struct {
	BackendState string
	Self         rawNode
	Peer         map[string]rawNode
}

type rawNode struct {
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

// SubnetRoutes is routes minus the default routes: what a subnet router advertises or has enabled.
func SubnetRoutes(routes []string) []string {
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
	for _, r := range SubnetRoutes(self.AllowedIPs) {
		if !slices.Contains(own, r) {
			out = append(out, r)
		}
	}
	return out
}

// AdvertiseRoutes sets this box's advertised subnet routes (`set --advertise-routes=<csv>`); empty withdraws them.
func AdvertiseRoutes(ctx context.Context, routes []string) error {
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
	cmd := execCommandContext(ctx, bin, append([]string{"set"}, flags...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// tailscaleBin resolves the installed CLI; ErrNoCLI when it is not on PATH.
func tailscaleBin() (string, error) {
	bin, err := lookPathReal("tailscale")
	if err != nil {
		return "", ErrNoCLI
	}
	return bin, nil
}

// Status parses `tailscale status --json`. ErrNoCLI when not on PATH; ErrNotRunning when the
// backend is not Running (the parsed Local is still returned in that case).
func Status(ctx context.Context) (Local, error) {
	bin, err := tailscaleBin()
	if err != nil {
		return Local{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()
	out, err := execCommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		return Local{}, fmt.Errorf("tailscale status --json: %w", err)
	}
	var raw rawStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		return Local{}, fmt.Errorf("tailscale status --json: %w", err)
	}
	local := Local{
		BackendState:     raw.BackendState,
		DNSName:          raw.Self.DNSName,
		IPs:              raw.Self.TailscaleIPs,
		ExitNodeOption:   raw.Self.ExitNodeOption,
		ExitNodeApproved: hasDefaultRoutes(raw.Self.AllowedIPs),
		SubnetRoutes:     subnetRoutesOf(raw.Self),
	}
	for _, p := range raw.Peer {
		local.Peers = append(local.Peers, Peer{
			DNSName: p.DNSName, IPs: p.TailscaleIPs, OS: p.OS, Online: p.Online,
			ExitNode: p.ExitNode, ExitNodeOption: p.ExitNodeOption, LastSeen: p.LastSeen,
		})
	}
	if raw.BackendState != "Running" {
		return local, ErrNotRunning
	}
	return local, nil
}

// Connect brings this box onto the tailnet: a no-op when already Running and, if exitNode is
// requested, its routes are already approved; otherwise `tailscale up --auth-key=file:<path>
// [--advertise-exit-node]`, the key written to a 0600 temp file (never an argv element, which
// `ps` would expose) and removed once the command exits.
func Connect(ctx context.Context, authKey string, exitNode bool) error {
	bin, err := tailscaleBin()
	if err != nil {
		return err
	}
	if st, err := Status(ctx); err == nil && st.BackendState == "Running" && (!exitNode || st.ExitNodeApproved) {
		return nil
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
	ctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
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
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		_ = os.Remove(f.Name()) // best-effort cleanup; the Chmod error is already being returned
		return "", err
	}
	if _, err := f.WriteString(authKey); err != nil {
		_ = os.Remove(f.Name()) // best-effort cleanup; the write error is already being returned
		return "", err
	}
	return f.Name(), nil
}

// AdvertiseExitNode toggles exit-node advertisement via `tailscale set --advertise-exit-node[=false]`.
func AdvertiseExitNode(ctx context.Context, on bool) error {
	flag := "--advertise-exit-node"
	if !on {
		flag += "=false"
	}
	return set(ctx, flag)
}

// UseExitNode routes this box's traffic through a peer exit node via `tailscale set --exit-node=<peer>`.
func UseExitNode(ctx context.Context, peer string) error {
	flags := []string{"--exit-node=" + peer}
	if peer != "" {
		flags = append(flags, "--exit-node-allow-lan-access=true")
	}
	return set(ctx, flags...)
}

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

// Machines merges the admin API's device list with CLI status when a credential is present; falls
// back to CLI-only (self + peers) when NewClient returns ErrNoCredential.
func Machines(ctx context.Context) ([]Machine, error) {
	client, err := NewClient()
	if errors.Is(err, ErrNoCredential) {
		return machinesFromStatus(ctx)
	}
	if err != nil {
		return nil, err
	}
	devices, err := client.Devices(ctx)
	if err != nil {
		return nil, err
	}
	st, _ := Status(ctx) // best-effort online/exit-node overlay; a status error leaves the API-only view
	byAddr := map[string]Peer{}
	for _, p := range st.Peers {
		for _, a := range p.IPs {
			byAddr[a] = p
		}
	}
	// This node is never in its own peer list: Running is its online state.
	for _, a := range st.IPs {
		byAddr[a] = Peer{Online: st.BackendState == "Running", ExitNode: st.ExitNodeOption}
	}
	out := make([]Machine, 0, len(devices))
	for _, d := range devices {
		m := Machine{Name: d.Name, Addresses: d.Addresses, OS: d.OS, LastSeen: d.LastSeen, Tags: d.Tags, Source: "api"}
		for _, a := range d.Addresses {
			if p, ok := byAddr[a]; ok {
				m.Online, m.ExitNodeAdvertised = p.Online, p.ExitNodeOption || p.ExitNode
				break
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// machinesFromStatus is the no-credential fallback: self plus every CLI-visible peer.
func machinesFromStatus(ctx context.Context) ([]Machine, error) {
	st, err := Status(ctx)
	if err != nil && !errors.Is(err, ErrNotRunning) {
		return nil, err
	}
	out := []Machine{{Name: st.DNSName, Addresses: st.IPs, Online: st.BackendState == "Running", ExitNodeApproved: st.ExitNodeApproved, Source: "cli"}}
	for _, p := range st.Peers {
		out = append(out, Machine{Name: p.DNSName, Addresses: p.IPs, OS: p.OS, Online: p.Online, LastSeen: p.LastSeen, ExitNodeAdvertised: p.ExitNodeOption || p.ExitNode, Source: "cli"})
	}
	return out, nil
}
