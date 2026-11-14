package remote

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// Auto is the --host value ResolveAuto turns into a host name.
const Auto = "auto"

// HostEnv names the machine `auto` resolves to (process env, then vault env); unset, the tailnet is probed.
const HostEnv = "CLAUDE_REMOTE_HOST"

// autoHint closes every `auto` failure: a vault env value, set in configure's environment row, not sealed.
const autoHint = "name the machine: `claude-code configure` → environment, " + HostEnv

// autoProbeTimeout bounds the tailnet status read and each candidate's `claude-code version` over ssh.
var autoProbeTimeout = 5 * time.Second

// autoHostTTL is how long a probed answer is reused before the tailnet is asked again.
const autoHostTTL = 10 * time.Minute

// Seams of the auto resolver: tests stub all three, so nothing reads the vault, tailscale or ssh.
var (
	autoEnvHost = func() string {
		return cmp.Or(strings.TrimSpace(os.Getenv(HostEnv)), strings.TrimSpace(providers.CustomEnv()[HostEnv]))
	}
	autoPeers   = onlinePeers
	autoAnswers = answersVersion
)

// ResolveAuto turns `--host auto` or `--remote auto` into CLAUDE_REMOTE_HOST, else the one other tailnet machine that answers ssh.
func ResolveAuto(host string) (string, error) {
	if host != Auto {
		return host, nil
	}
	if h := autoEnvHost(); h != "" && h != Auto {
		return h, nil
	}
	if h := cachedAutoHost(time.Now()); h != "" {
		return h, nil
	}
	peers, err := autoPeers()
	if err != nil {
		return "", fmt.Errorf("auto: %w; %s", err, autoHint)
	}
	switch up := answering(peers); len(up) {
	case 1:
		_ = paths.WriteJSON(autoHostFile(), autoHost{Schema: autoHostSchema, At: time.Now(), Host: up[0]})
		return up[0], nil
	case 0:
		return "", fmt.Errorf("auto: no other online machine answers `claude-code version` over ssh (tried: %s); %s",
			cmp.Or(strings.Join(peers, ", "), "none online"), autoHint)
	default:
		return "", fmt.Errorf("auto: %d machines answer (%s); %s", len(up), strings.Join(up, ", "), autoHint)
	}
}

// IsSelf reports whether host names this machine (tailscale.IsLocalHost); `auto` and a blank host never do.
func IsSelf(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || host == Auto {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoProbeTimeout)
	defer cancel()
	return tailscale.IsLocalHost(ctx, host)
}

// LocalArgs is the verb after `flag <host>` when host names this machine, so the caller runs it here with no hop.
func LocalArgs(flag string, args []string) ([]string, bool) {
	host, rest, err := SplitHost(flag, args)
	if err != nil || !IsSelf(host) {
		return nil, false
	}
	return rest, true
}

// answering probes every peer at once and keeps, in order, the ones that answer.
func answering(peers []string) []string {
	ok := make([]bool, len(peers))
	var wg sync.WaitGroup
	for i, h := range peers {
		wg.Go(func() { ok[i] = autoAnswers(h) })
	}
	wg.Wait()
	var up []string
	for i, h := range peers {
		if ok[i] {
			up = append(up, h)
		}
	}
	return up
}

// onlinePeers lists the other tailnet machines that are online, by DNS name; this node is never among its peers.
func onlinePeers() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), autoProbeTimeout)
	defer cancel()
	st, err := tailscale.Status(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range st.Peers {
		if name := strings.TrimSuffix(p.DNSName, "."); p.Online && name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// answersVersion reports whether host runs `claude-code version` over a prompt-free ssh within autoProbeTimeout.
func answersVersion(host string) bool {
	argv, err := StreamArgs(host, PathPrefix+" exec "+textutil.QuotePath(HomePath(Bin()))+" version")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoProbeTimeout)
	defer cancel()
	return runCommand(exec.CommandContext(ctx, argv[0], argv[1:]...)) == nil
}

// autoHost is state/harness/remote/auto-host.json: the machine the last tailnet probe settled on.
type autoHost struct {
	Schema int       `json:"schema"`
	At     time.Time `json:"at"`
	Host   string    `json:"host"`
}

const autoHostSchema = 1

var autoHostFormat = schemas.Register(schemas.Format{
	Name: "remote-auto-host", Owner: "internal/network/remote", Kind: schemas.Cache, Version: autoHostSchema,
	Glob: autoHostFile,
	New:  func() any { return new(autoHost) },
	Keep: "rebuildable cache: a miss probes the tailnet again",
})

func autoHostFile() string { return filepath.Join(paths.StateDir(), "remote", "auto-host.json") }

// cachedAutoHost is the probed host when the cache is fresh, else "".
func cachedAutoHost(now time.Time) string {
	raw, err := os.ReadFile(autoHostFile())
	var c autoHost
	if err != nil || !schemas.DecodeOK(autoHostFormat, raw, &c) || paths.Stale(c.At, autoHostTTL, 0, now) {
		return ""
	}
	return c.Host
}
