package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

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

// Resend runs `agents ask --retry <holder> --recover` under the holder's parent and returns the new dispatch id.
func Resend(ctx context.Context, holder string) (string, error) {
	rec, err := LoadRecord(holder)
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
