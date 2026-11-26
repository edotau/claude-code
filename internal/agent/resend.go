package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// resendOutput runs the re-send command; tests replace it.
var resendOutput = func(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

const resendTimeout = time.Minute

// resumeChainCap bounds a walk back through resumed markers.
const resumeChainCap = 64

// ResumedFrom is the dispatch id a `--retry --recover` marker names id as resumed from, else "".
func ResumedFrom(id string) string {
	ents, _ := os.ReadDir(WatchDir())
	for _, e := range ents {
		from, ok := strings.CutPrefix(e.Name(), "resumed-")
		if !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(WatchDir(), e.Name())); err == nil && strings.TrimSpace(string(b)) == id {
			return from
		}
	}
	return ""
}

// ResumeChain lists the dispatches id was resumed from, nearest first.
func ResumeChain(id string) []string {
	var out []string
	for from := ResumedFrom(id); from != "" && len(out) < resumeChainCap; from = ResumedFrom(from) {
		if from == id {
			break
		}
		out = append(out, from)
	}
	return out
}

// ResendClaimed reports whether a recovery of holder is under way: its marker exists and was written recently.
func ResendClaimed(holder string) bool {
	m := WatchMarker("resumed", holder)
	fi, err := os.Stat(m)
	return m != "" && err == nil && time.Since(fi.ModTime()) < resendTimeout
}

// followable reports whether the dispatch id that holder resumed as is worth following: ok, live or not yet saved.
func followable(holder, id string) bool {
	rec, err := LoadRecord(id)
	if errors.Is(err, ErrNoRecord) {
		return ResendClaimed(holder) // the recovery is starting: its record is not written yet
	}
	return err == nil && (rec.Status == wire.StatusOK || rec.Status == "running" && jobLive(rec))
}

// ErrNotResent marks a re-send refused because the holder was cancelled by the user or ended on a budget.
var ErrNotResent = errors.New("not re-sent")

// ResendGate is the one gate of every automatic re-send: id's record read under its lock, a NoResend one refused.
func ResendGate(id string) (Record, error) {
	var rec Record
	path := filepath.Join(paths.DispatchDir(), id+".json")
	err := paths.WithFileLock(path, func() (rerr error) {
		rec, rerr = readRecord(path)
		return rerr
	})
	switch {
	case err != nil:
		return Record{}, err
	case rec.FailKind == FailCancelled:
		return rec, fmt.Errorf("%w: cancelled", ErrNotResent)
	case rec.NoResend(time.Now()):
		return rec, fmt.Errorf("%w: budget", ErrNotResent)
	}
	return rec, nil
}

// Resend runs `agents ask --retry <holder> --recover` under the holder's parent and returns the new dispatch id.
func Resend(ctx context.Context, holder string) (string, error) {
	rec, err := ResendGate(holder)
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Not cancelled with the caller: a retry cut off after its job spawned would leave a child nobody settles.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resendTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "agents", "ask", "--retry", holder, "--recover", "--background", "--format", "json")
	cmd.Env = append(os.Environ(), proc.DispatchIDEnv+"="+rec.Parent)
	out, err := resendOutput(cmd)
	var got struct {
		ID string `json:"id"`
	}
	if err == nil && (json.Unmarshal(out, &got) != nil || got.ID == "") {
		err = fmt.Errorf("no dispatch id in %d bytes of output", len(out))
	}
	if err != nil { // a recovery that launched anyway (timeout, lost claim) is named by its marker: follow it
		if won, ok := ResumedAs(holder); ok && followable(holder, won) {
			return won, nil
		}
		return "", err
	}
	return got.ID, nil
}
