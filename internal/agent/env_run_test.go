package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a real child under ChildCmd with a grandchild, When ctx is cancelled, Then no process stays in the group.
func TestChildCmdCancelLeavesNoProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are unix-only")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, _ := ChildCmd(ctx, "sh", []string{"-c", "sleep 60 & sleep 60; wait"}, nil, t.TempDir(), "", nil, false)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond)
	if err := exec.Command("kill", "-0", "--", "-"+strconv.Itoa(pgid)).Run(); err != nil {
		t.Fatalf("group %d not alive before cancel: %v", pgid, err)
	}
	cancel()
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) {
		t.Fatalf("wait = %v, want the kill's exit error", err)
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if out, err := exec.Command("kill", "-0", "--", "-"+strconv.Itoa(pgid)).CombinedOutput(); err != nil &&
			strings.Contains(string(out), "No such process") {
			return
		}
	}
	t.Errorf("process group %d still has members after cancel", pgid)
}

// An ambient but empty COWORKER_OVERLAY must not hide the vault's stored overlay from the captured snapshot.
func TestRunValuesAmbientEmptyOverlayKeepsStored(t *testing.T) {
	vaulttest.Use(t)
	const stored = `{"provider":"databricks","profile":"qa"}`
	vaulttest.Put(t, vault.Env, route.OverlayVar, stored)
	t.Setenv(route.OverlayVar, "")
	ctx, _ := RunValues(context.Background(), nil)
	if got := agentconfig.DispatchOverlayContext(ctx, ""); got != stored {
		t.Fatalf("DispatchOverlayContext = %q, want the stored overlay %q", got, stored)
	}
}

// Concurrent child launches keep independent snapshots and request-pinned egress without changing the parent.
func TestRunValuesConcurrentChildren(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	t.Setenv("PST_RUN", "parent")
	t.Setenv(egress.EnvHost, "parent-host")
	values := proc.ValuesFromEnv(os.Environ())
	start := make(chan struct{})
	errc := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func() {
			<-start
			ctx, copied := RunValues(context.Background(), values.With("PST_RUN", name))
			copied["PST_RUN"] = "mutated-after-capture"
			ctx = egress.WithHost(ctx, name+"-host")
			for _, agent := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
				cmd, _ := ChildCmd(ctx, "/bin/sh", []string{"-c", `printf '%s|%s' "$PST_RUN" "$HARNESS_EGRESS_HOST"`},
					nil, "", "", nil, agent != "claude")
				out, err := cmd.Output()
				if err != nil {
					errc <- err
					return
				}
				if string(out) != name+"|"+name+"-host" {
					errc <- fmt.Errorf("%s child got %q", agent, out)
					return
				}
			}
			direct := proc.ValuesFromEnv(EnvSpec{Mode: EnvClaude}.Environ(egress.WithHost(ctx, "")))
			if direct[egress.EnvHost] != "" {
				errc <- errors.New("explicit direct host lost")
				return
			}
			errc <- nil
		}()
	}
	close(start)
	for range 2 {
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}
	if os.Getenv("PST_RUN") != "parent" || os.Getenv(egress.EnvHost) != "parent-host" {
		t.Fatal("child snapshot changed the parent")
	}
}
