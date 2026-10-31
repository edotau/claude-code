package memory

import (
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// EmbedProfile is the first of the session and claude-record profiles that can embed, matching NewSemantic's order.
func EmbedProfile() string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	for _, name := range []string{providers.SessionProfile(), providers.AgentRecord("claude").Profile} {
		if p := reg.Providers[name]; p != nil && p.Preset == providers.PresetDatabricks {
			return name
		}
	}
	return ""
}

// RefreshEmbeddings starts a detached mint at most once per claim TTL; failed launches release the slot.
func RefreshEmbeddings(root, profile string, now time.Time) bool {
	if IsUpdateChild() || !safeEmbedProfile(profile) {
		return false
	}
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return false
	}
	if embedBackedOff(profile, now) {
		return false
	}
	stamp := filepath.Join(StateDir(), "embeddings-"+profile+".refresh")
	spawned := false
	_, _ = paths.TryFileLock(stamp+".lock", 0, func() error {
		if st, err := os.Stat(stamp); err == nil && now.Sub(st.ModTime()) < embedClaimTTL {
			return nil
		}
		if !embedRefreshDue(profile, now) {
			return nil
		}
		if err := paths.AtomicWrite(stamp, nil, 0o600); err != nil {
			return err
		}
		spawned = spawnEmbedRefresh(root, profile)
		if !spawned {
			_ = os.Remove(stamp)
		}
		return nil
	})
	return spawned
}

// embedBackedOff reports a recent endpoint timeout, when a mint would only fail and retry.
func embedBackedOff(profile string, now time.Time) bool {
	st, err := os.Stat(filepath.Join(StateDir(), embedCachePre+profile+".backoff"))
	return err == nil && now.Sub(st.ModTime()) < embedBackoff
}

// spawnEmbedRefresh uses the mint-only index path so automatic refresh never settles the bank.
func spawnEmbedRefresh(root, profile string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	args := []string{"memory", "index", "--mint-only", "--repo", root}
	return proc.SpawnDetached(self, args, root, nil, providers.EnvSessionProfile+"="+profile, UpdateChildEnv+"=1")
}
