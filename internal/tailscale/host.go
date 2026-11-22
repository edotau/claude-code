package tailscale

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

var (
	selfNames          = paths.NewMemo[struct{}, []string]("tailscale-self")
	selfLookup         sync.Mutex
	selfFailureBackoff = 30 * time.Second
)

// IsLocalHost reports whether host names this machine: localhost, its hostname, or its tailnet name/IPs.
func IsLocalHost(ctx context.Context, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.Contains(h, "@") { // an explicit ssh user is a request to ssh, never to run here
		return false
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || isOwnHostname(h) {
		return true
	}
	if ctx.Err() != nil {
		// A canceled caller can read settled names, but must not wait behind a live lookup.
		if !selfLookup.TryLock() {
			return false
		}
	} else {
		selfLookup.Lock()
	}
	defer selfLookup.Unlock()
	names, _ := selfNames.Do(struct{}{}, func(_ []string, err error) time.Duration {
		if err != nil {
			return selfFailureBackoff
		}
		return 0
	}, func() ([]string, error) { return selfTailnetNames(ctx) })
	return slices.Contains(names, h)
}

// selfTailnetNames lists this node's tailnet IPs and names; an error discards any names.
func selfTailnetNames(ctx context.Context) ([]string, error) {
	st, err := Status(ctx)
	if err != nil {
		return nil, err
	}
	names := slices.Clone(st.IPs)
	if dns := strings.ToLower(strings.TrimSuffix(st.DNSName, ".")); dns != "" {
		short, _, _ := strings.Cut(dns, ".")
		names = append(names, dns, short)
	}
	return names, nil
}

// isOwnHostname reports whether h is this machine's OS hostname or its short form.
func isOwnHostname(h string) bool {
	n, err := os.Hostname()
	if err != nil {
		return false
	}
	n = strings.ToLower(strings.TrimSuffix(n, "."))
	short, _, _ := strings.Cut(n, ".")
	return h != "" && (h == n || h == short)
}

// ResolveHost maps a short tailnet name to its MagicDNS name (cache first, then live); others pass through to ssh.
func ResolveHost(ctx context.Context, host string) (string, error) {
	if err := textutil.ValidHost(host); err != nil {
		return "", err
	}
	if host == "localhost" || strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return host, nil // FQDN, MagicDNS name or IP: nothing to resolve
	}
	if name, ok := matchMachine(CachedMachineNames(), host); ok {
		return name, nil
	}
	machines, err := Machines(ctx)
	if err != nil {
		return host, nil
	}
	names := make([]string, len(machines))
	for i, m := range machines {
		names[i] = m.Name
	}
	if name, ok := matchMachine(names, host); ok {
		return name, nil
	}
	return host, nil
}

// matchMachine returns the first name equal to host, or whose short (pre-dot) form is, ignoring case.
func matchMachine(names []string, host string) (string, bool) {
	for _, n := range names {
		if strings.EqualFold(n, host) {
			return n, true
		}
		if short, _, ok := strings.Cut(n, "."); ok && strings.EqualFold(short, host) {
			return n, true
		}
	}
	return "", false
}

// machinesCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const machinesCacheSchema = 1

var machinesCacheFormat = schemas.Register(schemas.Format{
	Name: "tailscale-machines", Owner: "internal/tailscale", Kind: schemas.Cache,
	Glob:    func() string { return machinesCachePath() },
	New:     func() any { return &machinesCache{} },
	Version: machinesCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// machinesCachePath holds the last --machines listing for completion, which never calls the network.
func machinesCachePath() string { return filepath.Join(paths.StateDir(), "tailscale-machines.json") }

// machinesCacheTTL bounds how stale the cached machine list may be before completion ignores it.
const machinesCacheTTL = 10 * time.Minute

type machinesCache struct {
	Schema int       `json:"schema"`
	At     time.Time `json:"at"`
	Names  []string  `json:"names"`
}

// WriteMachinesCache records a successful --machines listing; best-effort, errors are dropped.
func WriteMachinesCache(names []string) {
	_ = paths.WriteJSONNoSync(machinesCachePath(), machinesCache{Schema: machinesCacheSchema, At: time.Now(), Names: names})
}

// CachedMachineNames reads the --machines cache when fresh, else nil: Tab completion has no budget for calls.
func CachedMachineNames() []string {
	data, err := os.ReadFile(machinesCachePath())
	if err != nil {
		return nil
	}
	var c machinesCache
	if !schemas.DecodeOK(machinesCacheFormat, data, &c) {
		return nil
	}
	if paths.Stale(c.At, machinesCacheTTL, 0, time.Now()) { // a future At (clock skew) is stale
		return nil
	}
	return c.Names
}
