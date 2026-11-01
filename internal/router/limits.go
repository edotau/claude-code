package router

import (
	"cmp"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc/inflight"
	"github.com/edotau/claude-code/internal/providers"
)

// CurrentLimits reads the registry and router state; any error fails open to Base.
func CurrentLimits() inflight.Limits {
	cpus := runtime.NumCPU()
	reg, err := providers.Load()
	if err != nil {
		return inflight.ComputeLimits(nil, nil, cpus)
	}
	state, now := LoadState(), time.Now()
	var available []string
	override := map[string]int{}
	for name, p := range reg.Providers {
		account := ProviderAccount(name)
		if p.Concurrency > 0 {
			override[account] = p.Concurrency
		}
		if !providers.CredentialPresent(p) || state[account].Status(now) == StateExhausted {
			continue
		}
		if _, marked := state.AuthMarked(account, now); marked {
			continue
		}
		available = append(available, account)
	}
	slices.Sort(available)
	return inflight.ComputeLimits(available, override, cpus)
}

// SessionAccount is the account this session bills: the loopback /p/<name>/ profile, else pin, else the default.
func SessionAccount() string {
	name := cmp.Or(providers.SessionProfile(), providers.AgentRecord("claude").Profile)
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
	return ProviderAccount(name)
}
