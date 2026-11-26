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
		if next, ok := ResumedAs(from); ok && next == id {
			return from
		}
	}
	names, _ := recordNames(paths.DispatchDir())
	for _, name := range names {
		r, err := readRecord(filepath.Join(paths.DispatchDir(), name))
		if err == nil && r.Resumer.phase() == claimAdopted && r.Resumer.Successor == id {
			return r.ID
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

// ResendClaimed reports whether a recovery of holder is under way: a live or launched claim made recently.
func ResendClaimed(holder string) bool {
	rec, ok := parentRecord(holder)
	return ok && rec.claimBlocks() && time.Since(rec.Resumer.Time) < resendTimeout
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

// resendRefusal is the refusal a record carries (cancelled, budget end), nil when it may be re-sent.
func resendRefusal(rec Record) error {
	switch {
	case rec.cancelled():
		return fmt.Errorf("%w: cancelled", ErrNotResent)
	case rec.LostBudgetEnd(time.Now()):
		return fmt.Errorf("%w: budget", ErrNotResent)
	}
	return nil
}

// withHolderLock runs fn under holder's dispatch record lock, shared by cancel, claim and re-send.
func withHolderLock(id string, fn func(Record) error) error {
	path := filepath.Join(paths.DispatchDir(), id+".json")
	return paths.WithFileLock(path, func() error {
		rec, err := readRecord(path)
		if err != nil {
			return err
		}
		return fn(rec)
	})
}

// ResendGate is the one gate of every automatic re-send: id's record read under its lock, a NoResend one refused.
func ResendGate(id string) (Record, error) {
	var rec Record
	err := withHolderLock(id, func(r Record) error {
		rec = r
		return resendRefusal(r)
	})
	if err != nil && !errors.Is(err, ErrNotResent) {
		return Record{}, err
	}
	return rec, err
}

// ErrHolderLive marks a claim refused because the holder's runner is still alive.
var ErrHolderLive = errors.New("holder still running")

// ErrAlreadyResumed marks a claim refused because another session already took the holder over.
var ErrAlreadyResumed = errors.New("already resumed")

// ResumeNonceEnv carries a launching claim's nonce to the successor, which adopts the claim with it.
const ResumeNonceEnv = "HARNESS_RESUME_NONCE"

// underLockHook is called with the operation name while a record lock is held; tests force interleavings through it.
var underLockHook = func(string) {}

// taken reports a re-send that took r over: a live claim in its record, else a legacy named marker.
func (r Record) taken() bool {
	if r.Resumer != nil {
		return r.claimBlocks()
	}
	_, named := markerSuccessor(r.ID)
	return named
}

// ClaimedBy reports whether r holds the live claim of attempt nonce.
func (r Record) ClaimedBy(nonce string) bool {
	return nonce != "" && r.Resumer != nil && r.Resumer.Nonce == nonce && r.claimBlocks()
}

// ClaimResend gates takeover under holder's lock (live, cancel/budget, prior claim) and returns the claim nonce.
func ClaimResend(holder string) (Record, string, error) {
	if WatchMarker("resumed", holder) == "" {
		return Record{}, "", fmt.Errorf("dispatch id %q cannot be claimed", holder)
	}
	var rec Record
	nonce := newToken()
	err := transition(holder, "claim-resend", func(r Record) (Record, error) {
		rec = r
		if holderLive(r) {
			return r, fmt.Errorf("dispatch %s: %w", holder, ErrHolderLive)
		}
		if err := resendRefusal(r); err != nil {
			return r, err
		}
		if id, ok := r.resumedAs(); ok {
			return r, fmt.Errorf("dispatch %s already recovered as %s: %w", holder, id, ErrAlreadyResumed)
		}
		var err error
		if r, err = r.voidClaim(); err != nil {
			return r, err
		}
		if r.Resumer.phase() != claimNone {
			return r, fmt.Errorf("dispatch %s is already being recovered by another session: %w", holder, ErrAlreadyResumed)
		}
		return r.stepClaim(claimClaimed, nonce, "")
	}, nil)
	return rec, nonce, err
}

// MarkLaunching records, before the successor detaches, that a dead claimer may already have launched it.
func MarkLaunching(holder, nonce string) error {
	return transition(holder, "claim-launch", func(r Record) (Record, error) {
		return r.stepClaim(claimLaunching, nonce, "")
	}, nil)
}

// AdoptResend commits adoption; the marker is an optional index, so its failure cannot abort the adopted successor.
func AdoptResend(holder, nonce, successor string) error {
	marker := WatchMarker("resumed", holder)
	if marker == "" || successor == "" {
		return fmt.Errorf("dispatch id %q cannot be adopted as %q", holder, successor)
	}
	return transition(holder, "claim-adopt", func(r Record) (Record, error) {
		return r.stepClaim(claimAdopted, nonce, successor)
	}, func(Record) error {
		if err := writeResumedMarker(marker, successor); err != nil {
			fmt.Fprintf(NoticeOut, "dispatch %s adopted as %s; marker index not saved: %v\n", holder, successor, err)
		}
		return nil
	})
}

// writeResumedMarker is the pointer readers follow to a holder's successor; the claim in the record stays the truth.
func writeResumedMarker(marker, successor string) error {
	return paths.AtomicWriteNoSync(marker, []byte(successor+"\n"), 0o600) // readers never see a torn id
}

// ReleaseResend drops attempt nonce's claim before any launch, so the holder can be taken over again.
func ReleaseResend(holder, nonce string) error {
	return transition(holder, "claim-release", func(r Record) (Record, error) {
		return r.stepClaim(claimNone, nonce, "")
	}, nil)
}

// RollbackResend undoes this process's adoption as successor after its spawn failed: claim and marker go.
func RollbackResend(holder, successor string) error {
	marker := WatchMarker("resumed", holder)
	if marker == "" {
		return fmt.Errorf("dispatch id %q cannot be rolled back", holder)
	}
	return transition(holder, "claim-rollback", func(r Record) (Record, error) {
		return r.stepClaim(claimNone, "", successor)
	}, func(Record) error {
		if err := os.Remove(marker); !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
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
