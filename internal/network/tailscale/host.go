package tailscale

import (
	"context"
	"encoding/json"
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
	selfMu             sync.Mutex
	selfReady          bool
	selfNames          []string
	selfRetryAt        time.Time
	selfFailureBackoff = 30 * time.Second
	now                = time.Now
)

// IsLocalHost reports whether host names this machine: localhost, its hostname, or its tailnet name/IPs.
func IsLocalHost(ctx context.Context, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if _, at, ok := strings.Cut(h, "@"); ok {
		h = at
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || isOwnHostname(h) {
		return true
	}
	selfMu.Lock()
	if selfReady || now().Before(selfRetryAt) {
		local := slices.Contains(selfNames, h)
		selfMu.Unlock()
		return local
	}
	selfMu.Unlock()
	st, err := Status(ctx)
	selfMu.Lock()
	defer selfMu.Unlock()
	if !selfReady {
		recordSelf(st, err)
	}
	return slices.Contains(selfNames, h)
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

// recordSelf caches this node's tailnet names after a status call, or starts the failure backoff; selfMu is held.
func recordSelf(st Local, err error) {
	if err != nil {
		selfRetryAt = now().Add(selfFailureBackoff)
		return
	}
	selfReady = true
	selfNames = append(selfNames, st.IPs...)
	if dns := strings.ToLower(strings.TrimSuffix(st.DNSName, ".")); dns != "" {
		short, _, _ := strings.Cut(dns, ".")
		selfNames = append(selfNames, dns, short)
	}
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
	Name: "tailscale-machines", Owner: "internal/network/tailscale", Kind: schemas.Cache,
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
	data, err := json.Marshal(machinesCache{Schema: machinesCacheSchema, At: time.Now(), Names: names})
	if err != nil {
		return
	}
	_ = os.MkdirAll(paths.StateDir(), 0o700)
	_ = paths.AtomicWrite(machinesCachePath(), data, 0o600)
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
	if age := time.Since(c.At); age < 0 || age > machinesCacheTTL { // future At (clock skew) is stale
		return nil
	}
	return c.Names
}
