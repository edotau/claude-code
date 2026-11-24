package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/proc"
)

// leavesPoll is the interval of the leaf watch; leavesGrace spares a leaf that has only just started.
var (
	leavesPoll  = 3 * time.Second
	leavesGrace = 10 * time.Second
)

const resendTimeout = time.Minute

// watchLeaves watches owner contract id's leaves while its run lasts; the returned func stops the watch.
func watchLeaves(ctx context.Context, id string) func() {
	if c, err := contract.Load(id); err != nil || !slices.Contains(c.Terms, contract.TermLeaves) {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for t := time.NewTicker(leavesPoll); ; {
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				watchOnce(ctx, id, resend)
			}
		}
	}()
	return func() { cancel(); <-done }
}

// watchOnce re-sends each lost leaf of owner once, to a new holder; a second death fails it owner-dead.
func watchOnce(ctx context.Context, owner string, send func(context.Context, contract.Contract) (string, error)) {
	c, err := contract.Load(owner)
	if err != nil {
		return
	}
	for _, id := range c.Children {
		k, err := contract.Load(id)
		if err != nil || k.State() != contract.StateWorking || !leafLost(k) {
			continue
		}
		resendLeaf(ctx, k, send)
	}
}

// resendLeaf hands lost leaf k to a fresh dispatch once; a leaf already re-sent, or one that cannot be, fails.
func resendLeaf(ctx context.Context, k contract.Contract, send func(context.Context, contract.Contract) (string, error)) {
	holder := k.Holders[len(k.Holders)-1]
	if len(k.Holders) > 1 {
		failLeaf(k.ID, holder)
		return
	}
	next, err := send(ctx, k)
	if won, ok := agent.ResumedAs(holder); err != nil && ok { // the watchdog resumed it first: follow its dispatch
		next, err = won, nil
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: leaf %s: re-send: %v\n", k.ID, err)
		failLeaf(k.ID, holder)
		return
	}
	_, err = contract.Update(k.ID, func(k contract.Contract) (contract.Contract, error) {
		if k.State() != contract.StateWorking || k.Holders[len(k.Holders)-1] != holder {
			return k, nil
		}
		return k.Reassign(next)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: leaf %s: re-assign to %s: %v\n", k.ID, next, err)
	}
}

// failLeaf fails leaf id owner-dead when holder is still its working holder.
func failLeaf(id, holder string) {
	_, err := contract.Update(id, func(k contract.Contract) (contract.Contract, error) {
		if k.State() != contract.StateWorking || k.Holders[len(k.Holders)-1] != holder {
			return k, nil
		}
		return k.Fail(contract.ReasonOwnerDead)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: leaf %s: fail: %v\n", id, err)
	}
}

// leafLost reports whether k's current holder is no longer running; a record not yet written is spared the grace.
func leafLost(k contract.Contract) bool {
	rec, err := agent.LoadRecord(k.Holders[len(k.Holders)-1])
	if err != nil {
		return errors.Is(err, agent.ErrNoRecord) && contractAge(k.ID) > leavesGrace
	}
	if rec.Status == wire.StatusOK { // it finished: a failed settle is not a death, and a re-run would repeat its edits
		return false
	}
	return (rec.Status != "running" || !rec.Alive()) && time.Since(rec.Time) > leavesGrace
}

func contractAge(id string) time.Duration {
	p, err := contract.Path(id)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return time.Since(fi.ModTime())
}

// resendEnv names the contract a re-sent dispatch takes over; heldID reads it, so no scan races the watch's re-assign.
const resendEnv = "HARNESS_RESEND_CONTRACT"

// resend runs `agents ask --retry <holder> --recover` under the holder's parent and returns the new dispatch id.
func resend(ctx context.Context, k contract.Contract) (string, error) {
	holder := k.Holders[len(k.Holders)-1]
	rec, err := agent.LoadRecord(holder)
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Not cancelled with the watch: a retry cut off after its job spawned would leave a child nobody settles.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resendTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "agents", "ask", "--retry", holder, "--recover", "--background", "--format", "json")
	cmd.Env = append(os.Environ(), proc.DispatchIDEnv+"="+rec.Parent, resendEnv+"="+k.ID)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.ID == "" {
		return "", fmt.Errorf("no dispatch id in %d bytes of output", len(out))
	}
	return got.ID, nil
}
