package worktree

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// landRunName keys the one lease a land holds per checkout; every session's land queues on it.
const landRunName = "(land-run)"

// withLandRun runs fn as the only land of repo, waiting in line behind another; fn gets how long it waited.
func withLandRun(ctx context.Context, repo *Repo, lane string, out io.Writer, fn func(waited time.Duration) error) error {
	lease := operationLeasePath(repo.Main, landRunName)
	if err := os.MkdirAll(filepath.Dir(lease), 0o755); err != nil {
		return err
	}
	who := cmp.Or(os.Getenv(LandWaitEnv), "manual land")
	start, announced := nowFn(), false
	for {
		ran, err := paths.TryFileLock(lease, 0, func() error {
			_ = paths.AtomicWrite(lease+".who", []byte(strconv.Itoa(os.Getpid())+"\n"+who+"\n"), 0o600)
			return fn(nowFn().Sub(start))
		})
		if err != nil || ran {
			return err
		}
		pid, by := leaseHolder(lease + ".who")
		if !announced {
			fmt.Fprintf(out, "land %s: another land is running (pid %s, %s); waiting in line\n", lane, pid, by)
			announced = true
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if nowFn().Sub(start) > maxLandWait {
			return fmt.Errorf("worktree land: another land is running (pid %s, %s) after waiting %s", pid, by, maxLandWait)
		}
		sleepFn(pollInterval)
	}
}
