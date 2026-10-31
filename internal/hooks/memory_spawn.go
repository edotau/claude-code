package hooks

import (
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/agent/preflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// spawnUpdate is the detached-worker seam; tests replace it.
var spawnUpdate = func(transcriptPath, cwd, sessionID string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "update-"+time.Now().Format("20060102-150405")+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	args := []string{"memory", "update", "--transcript", transcriptPath, "--cwd", cwd, "--session", sessionID}
	if !proc.SpawnDetached(self, args, cwd, log) {
		return false
	}
	_ = memory.WriteUpdateResult(sessionID, memory.UpdateResult{Status: memory.UpdatePending, Time: time.Now(), Log: logPath})
	return true
}

// servedRefreshDue reports whether served routing is on and an in-use Databricks profile has no fresh listing.
func servedRefreshDue(now time.Time) bool {
	if !providers.ServedRoutingOn() {
		return false
	}
	reg, err := providers.Load()
	return err == nil && preflight.Stale(reg, now) && preflight.Claim(now)
}

// spawnServed is the detached `preflight --refresh` seam; tests replace it.
var spawnServed = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	return proc.SpawnDetached(self, []string{"preflight", "--refresh"}, cwd, null)
}

// spawnDream is the detached memory-dream seam; tests replace it.
var spawnDream = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "dream-"+time.Now().Format("20060102-150405")+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	return proc.SpawnDetached(self, []string{"memory", "dream", "--cwd", cwd}, cwd, log)
}

// refreshEmbeddings schedules only stale vector mints; no endpoint work runs in the hook.
func refreshEmbeddings(cwd string) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if memory.IsUpdateChild() || !memory.HasBank(cwd) {
		return
	}
	if profile := memory.EmbedProfile(); profile != "" {
		memory.RefreshEmbeddings(cwd, profile, time.Now())
	}
}
