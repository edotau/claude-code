package agent

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// ErrBadTransition marks a state change refused: not an edge of the table, or not the actor's to make.
var ErrBadTransition = errors.New("transition refused")

// ErrCancelling marks a cancel refused because another live canceller is mid-flight on the record.
var ErrCancelling = errors.New("cancel already in progress")

// CancelPhase is the cancel state: none -> cancelling -> signalled -> cancelled.
type CancelPhase string

const (
	cancelNone       CancelPhase = ""
	cancelCancelling CancelPhase = "cancelling" // owned by a canceller that has not signalled yet
	cancelSignalled  CancelPhase = "signalled"  // written before the signal is sent: never voided or cleared
	cancelDone       CancelPhase = "cancelled"  // the run ended stopped, or finished after a signal
)

// CancelInfo is the cancel state and, while cancelling, its owner.
type CancelInfo struct {
	Phase CancelPhase `json:"phase"`
	Token string      `json:"token,omitempty"` // the canceller's attempt
	PID   int         `json:"pid,omitempty"`
	Start string      `json:"start,omitempty"` // proc.StartTime of PID
}

var cancelEdges = map[[2]CancelPhase]bool{
	{cancelNone, cancelCancelling}:      true, // MarkCancelling
	{cancelCancelling, cancelNone}:      true, // the owner's rollback before any signal, or anyone voiding a dead owner
	{cancelCancelling, cancelSignalled}: true, // the owner, before it sends the signal
	{cancelCancelling, cancelDone}:      true, // the run ended stopped while a cancel was open
	{cancelSignalled, cancelDone}:       true, // the run ended after the signal, ok or not
	{cancelNone, cancelDone}:            true, // the run ended stopped with no cancel verb (a foreground stop)
}

func (c *CancelInfo) phase() CancelPhase {
	if c == nil {
		return cancelNone
	}
	return c.Phase
}

// void reports a cancelling whose canceller is gone: nothing is being cancelled.
func (c *CancelInfo) void() bool {
	return c.phase() == cancelCancelling && !identityLive(c.PID, c.Start)
}

// cancelPhase is the cancel state readers see: a dead canceller's cancelling is none, a stopped result is cancelled.
func (r Record) cancelPhase() CancelPhase {
	if c := r.Cancel; c.phase() != cancelNone && !c.void() {
		return c.Phase
	}
	if r.FailKind == FailCancelled {
		return cancelDone
	}
	return cancelNone
}

// cancelled reports a cancel in flight or done: the one cancel read every re-send and claim gate makes.
func (r Record) cancelled() bool { return r.cancelPhase() != cancelNone }

// cancelActor refuses an edge the actor may not take: a live canceller's rollback by another, a signal by a non-owner.
func cancelActor(from, to CancelPhase, c *CancelInfo, token string) bool {
	switch {
	case from == cancelCancelling && to == cancelNone:
		return c.Token == token || c.void()
	case from == cancelCancelling && to == cancelSignalled:
		return c.PID == os.Getpid()
	}
	return true
}

// stepCancel moves r's cancel state along one table edge; token names the actor of a rollback.
func (r Record) stepCancel(to CancelPhase, token string) (Record, error) {
	from, c := r.Cancel.phase(), r.Cancel
	if !cancelEdges[[2]CancelPhase{from, to}] || !cancelActor(from, to, c, token) {
		return r, fmt.Errorf("dispatch %s: cancel %q -> %q: %w", r.ID, from, to, ErrBadTransition)
	}
	switch to {
	case cancelNone:
		r.Cancel = nil
	case cancelCancelling:
		start, _ := proc.StartTime(os.Getpid())
		r.Cancel = &CancelInfo{Phase: to, Token: token, PID: os.Getpid(), Start: start}
	default:
		next := CancelInfo{}
		if c != nil {
			next = *c
		}
		next.Phase = to
		r.Cancel = &next
	}
	return r, nil
}

// settled closes the cancel state at a save: a dead canceller is voided, a stop or a signalled end is cancelled.
func (r Record) settled() Record {
	if c := r.Cancel; c.void() {
		r, _ = r.stepCancel(cancelNone, c.Token)
	}
	if r.Status == "running" {
		return r
	}
	p := r.Cancel.phase()
	if r.FailKind != FailCancelled && p != cancelSignalled && p != cancelDone {
		return r
	}
	if p != cancelDone {
		r, _ = r.stepCancel(cancelDone, "")
	}
	if r.Status != wire.StatusOK { // an ok result is still recorded, but the run counts as cancelled
		r.FailKind = FailCancelled
	}
	return r
}

// carryState is the rule of every ordinary save: cancel and claim come from disk, never from the writer's snapshot.
func carryState(prev, next Record) Record {
	next.Cancel, next.Resumer = prev.Cancel, prev.Resumer
	return next.settled()
}

// ClaimPhase is the re-send claim state: none -> claimed -> launching -> adopted.
type ClaimPhase string

const (
	claimNone      ClaimPhase = ""
	claimClaimed   ClaimPhase = "claimed"   // a claimer decided to take the holder over
	claimLaunching ClaimPhase = "launching" // written before the successor detaches: a dead claimer may have launched it
	claimAdopted   ClaimPhase = "adopted"   // the successor wrote its own id under the lock: the claim is carried on
)

// launchWindow is how long a launching claim stays unreclaimable even when its claimer is dead.
const launchWindow = time.Minute

// Claim is a re-send's takeover of a holder; the nonce tells overlapping attempts apart, even within one pid.
type Claim struct {
	Phase     ClaimPhase `json:"phase"`
	Nonce     string     `json:"nonce"`
	PID       int        `json:"pid"`                 // the claimer
	PIDStart  string     `json:"pid_start,omitempty"` // proc.StartTime of PID: tells a reused pid apart
	Deadline  time.Time  `json:"deadline,omitzero"`   // launching: no revert before it
	Successor string     `json:"successor,omitempty"` // adopted: the dispatch id the recovery runs as
	SuccPID   int        `json:"successor_pid,omitempty"`
	SuccStart string     `json:"successor_start,omitempty"`
	Time      time.Time  `json:"time"`
}

var claimEdges = map[[2]ClaimPhase]bool{
	{claimNone, claimClaimed}:      true, // ClaimResend
	{claimClaimed, claimLaunching}: true, // MarkLaunching
	{claimClaimed, claimAdopted}:   true, // the claimer is its own successor
	{claimLaunching, claimAdopted}: true, // the launched successor
	{claimClaimed, claimNone}:      true, // the claimer's release, or a dead claimer
	{claimLaunching, claimNone}:    true, // only a dead claimer past the deadline
	{claimAdopted, claimNone}:      true, // the adopter's rollback of a spawn that failed
}

func (c *Claim) phase() ClaimPhase {
	if c == nil {
		return claimNone
	}
	return c.Phase
}

// void reports a claim nobody carries on: claimed with a dead claimer, or launching past its deadline with one.
func (c *Claim) void(now time.Time) bool {
	switch c.phase() {
	case claimClaimed:
		return !identityLive(c.PID, c.PIDStart)
	case claimLaunching:
		return now.After(c.Deadline) && !identityLive(c.PID, c.PIDStart)
	}
	return false
}

// claimBlocks reports a claim that still bars another takeover of r.
func (r Record) claimBlocks() bool {
	return r.Resumer.phase() != claimNone && !r.Resumer.void(time.Now())
}

// claimActor reports whether the attempt (nonce, or the adopted successor of a rollback) may take the edge.
func claimActor(from, to ClaimPhase, c *Claim, nonce, successor string) bool {
	switch {
	case from == claimNone:
		return nonce != ""
	case to == claimNone && from == claimClaimed:
		return c.Nonce == nonce || c.void(time.Now())
	case to == claimNone && from == claimLaunching:
		return c.void(time.Now()) // an ambiguous launch is never released by its claimer
	case to == claimNone:
		return c.Successor == successor && c.SuccPID == os.Getpid()
	}
	return c.Nonce == nonce
}

// stepClaim moves r's claim along one table edge for attempt nonce; successor names an adoption's or rollback's id.
func (r Record) stepClaim(to ClaimPhase, nonce, successor string) (Record, error) {
	from, c, now := r.Resumer.phase(), r.Resumer, time.Now()
	if !claimEdges[[2]ClaimPhase{from, to}] || !claimActor(from, to, c, nonce, successor) {
		return r, fmt.Errorf("dispatch %s: claim %q -> %q: %w", r.ID, from, to, ErrBadTransition)
	}
	if to == claimNone {
		r.Resumer = &Claim{} // a durable none overrides any stale legacy marker
		return r, nil
	}
	start, _ := proc.StartTime(os.Getpid())
	next := Claim{}
	if c != nil {
		next = *c
	}
	switch to {
	case claimClaimed:
		next = Claim{Nonce: nonce, PID: os.Getpid(), PIDStart: start}
	case claimLaunching:
		next.Deadline = now.Add(launchWindow)
	case claimAdopted:
		next.Successor, next.SuccPID, next.SuccStart = successor, os.Getpid(), start
	}
	next.Phase, next.Time = to, now
	r.Resumer = &next
	return r, nil
}

// voidClaim drops a claim nobody carries on, along the table's void edges.
func (r Record) voidClaim() (Record, error) {
	if r.Resumer.void(time.Now()) {
		return r.stepClaim(claimNone, "", "")
	}
	return r, nil
}

// procStart reads a process start time; tests swap it to force an unreadable one.
var procStart = proc.StartTime

// identityLive reports pid alive with start time start; an unreadable start time counts live (no duplicate run).
func identityLive(pid int, start string) bool {
	if pid <= 0 {
		return false
	}
	got, err := procStart(pid)
	if errors.Is(err, proc.ErrNoProcess) {
		return false
	}
	return err != nil || start == "" || got == start
}

// holderLive reports a running record whose runner is alive by pid and start time; pid-less lives one minute.
func holderLive(r Record) bool {
	if r.Status != "running" {
		return false
	}
	if r.PID <= 0 {
		return r.Alive()
	}
	return identityLive(r.PID, r.PIDStart)
}

var tokenSeq atomic.Uint64

// newToken is a unique attempt id: pid, clock and a counter.
func newToken() string {
	return fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), tokenSeq.Add(1))
}

// transition is the one mutator of cancel and claim state: step, save, then after, all under id's record lock.
func transition(id, op string, step func(Record) (Record, error), after func(Record) error) error {
	return withHolderLock(id, func(r Record) error {
		underLockHook(op)
		next, err := step(r)
		if err != nil {
			return err
		}
		if err := next.settled().saveLocked(); err != nil {
			return err
		}
		if after != nil {
			return after(next)
		}
		return nil
	})
}

// MarkCancelling opens id's cancel before any signal and returns its token; a signalled or ended stop returns "".
func MarkCancelling(id string) (string, error) {
	token := ""
	err := transition(id, "cancel-mark", func(r Record) (Record, error) {
		if r.Status != "running" {
			return r, nil
		}
		if c := r.Cancel; c.void() {
			r, _ = r.stepCancel(cancelNone, c.Token) // anyone may void a dead canceller
		}
		switch r.Cancel.phase() {
		case cancelNone:
			token = newToken()
			return r.stepCancel(cancelCancelling, token)
		case cancelCancelling:
			return r, fmt.Errorf("dispatch %s: %w", id, ErrCancelling)
		}
		return r, nil
	}, nil)
	return token, err
}

// signalCancel records the cancel as signalled before the signal is sent, so a death in between reads as cancelled.
func signalCancel(id string) error {
	return transition(id, "cancel-signal", func(r Record) (Record, error) {
		if p := r.Cancel.phase(); p == cancelSignalled || p == cancelDone {
			return r, nil
		}
		return r.stepCancel(cancelSignalled, "")
	}, nil)
}

// ClearCancelling rolls back the cancelling that token opened while no signal was recorded; a later state stays.
func ClearCancelling(id, token string) error {
	return transition(id, "cancel-clear", func(r Record) (Record, error) {
		if r.Cancel.phase() != cancelCancelling {
			return r, nil
		}
		return r.stepCancel(cancelNone, token)
	}, nil)
}
