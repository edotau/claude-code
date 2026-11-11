package memory

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// logStampLayout timestamps detached worker log names.
const logStampLayout = "20060102-150405"

// spawnDetached is the process-launch seam under spawnSelf; tests replace it.
var spawnDetached = proc.SpawnDetached

// spawnSelf relaunches this binary detached in dir; a non-empty logName appends its output to StateDir()/logName.
func spawnSelf(args []string, dir, logName string, env ...string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	var out *os.File
	if logName != "" {
		if os.MkdirAll(StateDir(), 0o700) != nil {
			return false
		}
		out, err = os.OpenFile(filepath.Join(StateDir(), logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return false
		}
		defer out.Close()
	}
	return spawnDetached(self, args, dir, out, env...)
}

// spawner holds the detached-worker seams; tests swap the whole struct through stubSpawns.
type spawner struct {
	update  func(transcriptPath, cwd, sessionID string) bool
	watcher func(args []string, cwd string) bool
	dream   func(cwd string) bool
	catchup func(cwd, sessionID string) bool
}

var spawn = spawner{update: spawnUpdate, watcher: spawnWatcher, dream: spawnDream, catchup: spawnCatchup}

// spawnUpdate launches `memory update` and records the session's result as pending on success.
func spawnUpdate(transcriptPath, cwd, sessionID string) bool {
	logName := "update-" + time.Now().Format(logStampLayout) + ".log"
	args := []string{"memory", "update", "--transcript", transcriptPath, "--cwd", cwd, "--session", sessionID}
	if !spawnSelf(args, cwd, logName) {
		return false
	}
	logPath := filepath.Join(StateDir(), logName)
	_ = writeUpdateResult(sessionID, UpdateResult{Status: updatePending, Time: time.Now(), Log: logPath})
	return true
}

// spawnWatcher launches `memory watch`; args[1] is the session ID naming its log.
func spawnWatcher(args []string, cwd string) bool {
	return spawnSelf(append([]string{"memory", "watch"}, args...), cwd, "watch-"+filepath.Base(args[1])+".log")
}

func spawnDream(cwd string) bool {
	return spawnSelf([]string{"memory", "dream", "--cwd", cwd}, cwd, "dream-"+time.Now().Format(logStampLayout)+".log")
}

func spawnCatchup(cwd, sessionID string) bool {
	args := []string{"memory", "catchup", "--skip", sessionID}
	return spawnSelf(args, cwd, "catchup-"+time.Now().Format(logStampLayout)+".log")
}

// Spawns counts the stubbed spawns tests read; atomic because the idle watcher calls its stub on its own goroutine.
type Spawns struct{ Update, Dream, Catchup atomic.Int64 }

// stubSpawns replaces every detached worker spawn (update ok-flagged, the rest succeed); restore undoes it.
func stubSpawns(ok bool) (s *Spawns, restore func()) {
	s = new(Spawns)
	prev := spawn
	spawn = spawner{
		update:  func(string, string, string) bool { s.Update.Add(1); return ok },
		watcher: func([]string, string) bool { return true },
		dream:   func(string) bool { s.Dream.Add(1); return true },
		catchup: func(string, string) bool { s.Catchup.Add(1); return true },
	}
	return s, func() { spawn = prev }
}

// StubSpawn is stubSpawns restored when tb ends; tests use it so none relaunches the test binary.
func StubSpawn(tb testing.TB, ok bool) *Spawns {
	tb.Helper()
	s, restore := stubSpawns(ok)
	tb.Cleanup(restore)
	return s
}
