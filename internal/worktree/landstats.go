package worktree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Scope is how much the gate of one attempt covered.
type Scope string

// Gate scopes.
const (
	ScopeScoped Scope = "scoped"
	ScopeFull   Scope = "full"
	ScopeCustom Scope = "custom"
	ScopeNone   Scope = "none"
)

// Reval is how an attempt reused an earlier gate result after the target moved.
type Reval string

// Re-check modes.
const (
	RevalNone    Reval = "none"
	RevalReused  Reval = "reused"
	RevalPartial Reval = "partial"
	RevalFull    Reval = "full"
)

// Outcome is how one land attempt ended; Reason (the retry cause) uses the same values.
type Outcome string

// Attempt outcomes.
const (
	OutcomeLanded      Outcome = "landed"
	OutcomeGateFailed  Outcome = "gate_failed"
	OutcomeTargetMoved Outcome = "target_moved"
	OutcomeDirtyTarget Outcome = "dirty_target"
	OutcomeConflict    Outcome = "conflict"
	OutcomeLockBusy    Outcome = "lock_busy"
	OutcomeRefused     Outcome = "refused"
	OutcomeError       Outcome = "error"
)

// StageSeconds is the wall time of each stage of one attempt.
type StageSeconds struct {
	Wait    float64 `json:"wait"`
	Rebase  float64 `json:"rebase"`
	Build   float64 `json:"build"`
	Vet     float64 `json:"vet"`
	Test    float64 `json:"test"`
	Promote float64 `json:"promote"`
}

func (s StageSeconds) total() float64 {
	return s.Wait + s.Rebase + s.Build + s.Vet + s.Test + s.Promote
}

// LandAttempt is one line of land-attempts.jsonl: one `worktree land` attempt; Time is its start (unix seconds).
type LandAttempt struct {
	Schema        int          `json:"schema"`
	Time          int64        `json:"time"`
	Repo          string       `json:"repo"`
	Lane          string       `json:"lane"`
	Attempt       int          `json:"attempt"`
	By            string       `json:"by"`
	TargetOID     string       `json:"target_oid"`
	IntegratedOID string       `json:"integrated_oid"`
	Scope         Scope        `json:"scope"`
	Packages      int          `json:"packages"`
	Tests         int          `json:"tests"`                   // passed packages from bare go test ok lines
	FailedPkgs    []string     `json:"failed_pkgs,omitempty"`   // full import paths, from go test FAIL and build "# pkg" lines
	ChangedPkgs   []string     `json:"changed_pkgs,omitempty"`  // repo-relative dirs of the lane's changed Go packages
	ImporterPkgs  []string     `json:"importer_pkgs,omitempty"` // tested packages that are importers only
	Seconds       StageSeconds `json:"seconds"`
	Reval         Reval        `json:"reval"`
	Retested      int          `json:"retested"`
	Outcome       Outcome      `json:"outcome"`
	Reason        Outcome      `json:"reason"`
}

const (
	landAttemptSchema = 1
	// landKeepLines is how many newest lines a trim keeps; landMaxBytes is the size that triggers a trim.
	landKeepLines  = 2000
	landMaxBytes   = 4 << 20
	landRecordWait = 250 * time.Millisecond
)

var _ = schemas.Register(schemas.Format{
	Name: "land-attempts", Owner: "internal/worktree", Kind: schemas.Log, Version: landAttemptSchema,
	Glob: LandAttemptsFile, New: func() any { return &LandAttempt{} },
	Keep: "newest 2000 lines, trimmed when the file passes 4 MiB",
})

// LandAttemptsFile is the one attempt log every session appends to.
func LandAttemptsFile() string {
	return filepath.Join(paths.StateDir(), "worktree", "land-attempts.jsonl")
}

// RecordAttempt appends a to the log; a failure is a stderr warning and never fails the land.
func RecordAttempt(a LandAttempt) {
	if err := recordAttempt(LandAttemptsFile(), a); err != nil {
		fmt.Fprintln(os.Stderr, "warning: land attempt not recorded:", err)
	}
}

func recordAttempt(file string, a LandAttempt) error {
	a.Schema = landAttemptSchema
	line, err := json.Marshal(a)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return paths.WithFileLockWithin(file, landRecordWait, func() error {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.Write(line)
		fi, serr := f.Stat()
		if err := errors.Join(werr, f.Close()); err != nil {
			return err
		}
		if serr == nil && fi.Size() > landMaxBytes {
			return trimAttempts(file, landKeepLines)
		}
		return nil
	})
}

// trimAttempts rewrites file with only its newest keep lines; the caller holds the file lock.
func trimAttempts(file string, keep int) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	if len(lines) <= keep {
		return nil
	}
	out := bytes.Join(lines[len(lines)-keep:], nil)
	return paths.AtomicWrite(file, append(out, '\n'), 0o600)
}

// LandSummary is what LandStats reports over the lands inside its window.
type LandSummary struct {
	Lands           int
	P50, P95        time.Duration
	AttemptsPerLand float64
	MovedShare      float64 // share of lands that saw at least one target_moved attempt
	Reused          int     // lands whose last attempt reused an earlier gate result
}

// LandStats summarises the lands of repo ("" = every repo) that started within since of now.
func LandStats(repo string, since time.Duration) LandSummary {
	raw, err := os.ReadFile(LandAttemptsFile())
	if err != nil {
		return LandSummary{}
	}
	return summarize(parseAttempts(raw), repo, time.Now().Add(-since).Unix())
}

func parseAttempts(raw []byte) []LandAttempt {
	var out []LandAttempt
	for l := range bytes.SplitSeq(raw, []byte("\n")) {
		var a LandAttempt
		if json.Unmarshal(l, &a) == nil && a.Schema > 0 && a.Schema <= landAttemptSchema {
			out = append(out, a)
		}
	}
	return out
}

// summarize groups a lane's consecutive attempts up to its landed one; a lane still trying is not a land.
func summarize(all []LandAttempt, repo string, from int64) LandSummary {
	open := map[string][]LandAttempt{}
	var durs []time.Duration
	var s LandSummary
	var attempts, moved int
	for _, a := range all {
		if repo != "" && a.Repo != repo {
			continue
		}
		key := a.Repo + "\x00" + a.Lane
		open[key] = append(open[key], a)
		if a.Outcome != OutcomeLanded {
			continue
		}
		run := open[key]
		delete(open, key)
		if run[0].Time < from {
			continue
		}
		durs = append(durs, landDuration(run))
		attempts += len(run)
		if slices.ContainsFunc(run, func(r LandAttempt) bool { return r.Outcome == OutcomeTargetMoved }) {
			moved++
		}
		if a.Reval == RevalReused || a.Reval == RevalPartial {
			s.Reused++
		}
	}
	if s.Lands = len(durs); s.Lands == 0 {
		return s
	}
	slices.Sort(durs)
	s.P50, s.P95 = percentile(durs, 50), percentile(durs, 95)
	s.AttemptsPerLand = float64(attempts) / float64(s.Lands)
	s.MovedShare = float64(moved) / float64(s.Lands)
	return s
}

// landDuration is the last attempt's start minus the first's, plus the last attempt's own stage seconds.
func landDuration(run []LandAttempt) time.Duration {
	last := run[len(run)-1]
	return time.Duration(last.Time-run[0].Time)*time.Second + time.Duration(last.Seconds.total()*float64(time.Second))
}

const percentScale = 100

// percentile is the nearest-rank p-th percentile of sorted, which is not empty.
func percentile(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + percentScale - 1) / percentScale
	return sorted[max(rank, 1)-1]
}

// Line prints the summary as the stats row: `p50 3m10s p95 14m · 2.4 attempts/land · moved 61% · 43 lands (7d)`.
func (s LandSummary) Line(since time.Duration) string {
	window := fmt.Sprintf("%dd", int(since.Hours()/24))
	if s.Lands == 0 {
		return "no lands (" + window + ")"
	}
	return fmt.Sprintf("p50 %s p95 %s · %.1f attempts/land · moved %.0f%% · %d lands (%s)",
		shortDur(s.P50), shortDur(s.P95), s.AttemptsPerLand, s.MovedShare*100, s.Lands, window)
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
