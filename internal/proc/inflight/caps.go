package inflight

import (
	"cmp"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Base is the per-account cap on concurrent agents unless a profile's `concurrency` overrides it.
const Base = 12

// Limits are the caps: Base per account, Total across the machine, Override per account key.
type Limits struct {
	Base      int
	Total     int
	Available []string
	Override  map[string]int
}

// For is the cap of one account: its override, else Base.
func (l Limits) For(account string) int {
	if n := l.Override[account]; n > 0 {
		return n
	}
	return cmp.Or(l.Base, Base)
}

// ComputeLimits: Total = min(Base × max(len(available), 1), 4 × cpus); pure.
func ComputeLimits(available []string, override map[string]int, cpus int) Limits {
	total := Base * max(len(available), 1)
	if cpus > 0 {
		total = min(total, 4*cpus)
	}
	return Limits{Base: Base, Total: total, Available: available, Override: override}
}

// CurrentLimits reads the registry and router state; any error fails open to Base.
func CurrentLimits() Limits {
	cpus := runtime.NumCPU()
	reg, err := providers.Load()
	if err != nil {
		return ComputeLimits(nil, nil, cpus)
	}
	state, now := router.LoadState(), time.Now()
	var available []string
	override := map[string]int{}
	for name, p := range reg.Providers {
		account := router.ProviderAccount(name)
		if p.Concurrency > 0 {
			override[account] = p.Concurrency
		}
		if !providers.CredentialPresent(p) || state[account].Status(now) == router.StateExhausted {
			continue
		}
		if _, marked := state.AuthMarked(account, now); marked {
			continue
		}
		available = append(available, account)
	}
	slices.Sort(available)
	return ComputeLimits(available, override, cpus)
}

// SessionAccount is the account this session bills: the loopback /p/<name>/ profile, else pin, else the default.
func SessionAccount() string {
	name := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	if u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL")); err == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		if rest, ok := strings.CutPrefix(u.Path, "/p/"); ok {
			name, _, _ = strings.Cut(rest, "/")
		}
	}
	if name == "" {
		if reg, err := providers.Load(); err == nil {
			name = reg.Default
		}
	}
	return router.ProviderAccount(name)
}
