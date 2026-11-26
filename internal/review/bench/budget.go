package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Run kinds in the automatic-run ledger; limits are plan D2's: 15M tokens, 5 bench loops, 6 gate fixes a day.
const (
	KindBenchLoop   = "bench-loop"
	KindGateFix     = "gate-fix"
	dailyTokens     = 15_000_000
	dailyBenchLoops = 5
	dailyGateFixes  = 6
)

var budgetNow = time.Now

func budgetFile() string { return filepath.Join(benchRoot(), "budget.jsonl") }

type budgetRow struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	ID     string    `json:"id"`
	Tokens int       `json:"tokens"`
	// Dispatch rows are one dispatch's spend: the day's sum counts each id once, at its largest tokens.
	Dispatch string `json:"dispatch,omitempty"`
	// Started marks a worker reservation's one loop start; it is neither a run nor spend.
	Started bool `json:"started,omitempty"`
}

func budgetLimit(kind string) int {
	switch kind {
	case KindBenchLoop:
		return dailyBenchLoops
	case KindGateFix:
		return dailyGateFixes
	}
	return 0
}

func validBudgetRow(row budgetRow) bool {
	return !row.At.IsZero() && budgetLimit(row.Kind) > 0 && row.Tokens >= 0 &&
		(!row.Started || row.Tokens == 0 && row.Dispatch == "" && workerSHA.MatchString(row.ID))
}

// workerSHA is the only --reserved id the post-commit worker writes: a full commit sha.
var workerSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func readBudget() ([]budgetRow, error) {
	f, err := os.Open(budgetFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil { // the reason reaches the worker's marker, which never carries a path
		return nil, errors.New("budget ledger unreadable")
	}
	defer f.Close()
	var rows []budgetRow
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		var row budgetRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("budget ledger line %d: %w", line, err)
		}
		if !validBudgetRow(row) {
			return nil, fmt.Errorf("budget ledger line %d: invalid row", line)
		}
		rows = append(rows, row)
	}
	if sc.Err() != nil {
		return nil, errors.New("budget ledger unreadable")
	}
	return rows, nil
}

// allow refuses a run once today's count of kind or today's token total reaches its limit; a bad ledger refuses.
func allow(kind string, now time.Time) (ok bool, why string) {
	rows, err := readBudget()
	if err != nil {
		return false, err.Error()
	}
	why = budgetRefusal(rows, kind, "", now)
	return why == "", why
}

// budgetRefusal is why rows refuse one more run of kind today; a non-empty id already in the ledger is refused too.
func budgetRefusal(rows []budgetRow, kind, id string, now time.Time) string {
	limit := budgetLimit(kind)
	if limit == 0 {
		return "unknown budget kind"
	}
	if id != "" && slices.ContainsFunc(rows, func(row budgetRow) bool { return row.Kind == kind && row.ID == id }) {
		return "already launched for this commit"
	}
	runs, tokens := dayUse(rows, kind, now)
	if runs >= limit {
		return "daily " + kind + " count limit reached"
	}
	if tokens >= dailyTokens {
		return "daily token limit reached"
	}
	return ""
}

// tokenStop is the one-line reason the day's token total has reached the limit, or "".
func tokenStop(rows []budgetRow, now time.Time) string {
	_, tokens := dayUse(rows, KindBenchLoop, now)
	if tokens < dailyTokens {
		return ""
	}
	return fmt.Sprintf("daily token budget reached: %d/%d; resumes tomorrow", tokens, dailyTokens)
}

// ledgerTokenStop reads the ledger for tokenStop; a bad ledger is an error naming budget.jsonl.
func ledgerTokenStop(now time.Time) (string, error) {
	rows, err := readBudget()
	if err != nil {
		return "", fmt.Errorf("%w (inspect budget.jsonl)", err)
	}
	return tokenStop(rows, now), nil
}

const whyLoopLocked = "loop already locked"

// admitLoop gates a loop start: a worker run (reserved sha) consumes its row, a hand run reserves a slot; why refuses.
func admitLoop(reserved string, now time.Time) (why string, err error) {
	if reserved != "" {
		return consumeReservation(reserved, now)
	}
	rows, err := readBudget()
	if err != nil {
		return "", fmt.Errorf("%w (inspect budget.jsonl)", err)
	}
	if why = tokenStop(rows, now); why != "" {
		return why, nil
	}
	_, why = Reserve(KindBenchLoop, "hand-"+now.UTC().Format("20060102T150405.000000000"), now)
	return why, nil
}

// consumeReservation checks and marks sha's reservation started under the ledger lock Reserve holds: one start each.
func consumeReservation(sha string, now time.Time) (why string, err error) {
	if !workerSHA.MatchString(sha) {
		return "--reserved takes a worker's 40-hex commit sha", nil
	}
	data, err := json.Marshal(budgetRow{At: now, Kind: KindBenchLoop, ID: sha, Started: true})
	if err != nil {
		return "", errors.New("encode budget row")
	}
	var readErr error
	lockErr := paths.WithFileLock(budgetFile(), func() error {
		rows, err := readBudget()
		if err != nil {
			readErr = err
			return nil
		}
		if why = reservationRefusal(rows, sha, now); why != "" {
			return nil
		}
		if loopLocked() { // keep the reservation for no start: runDriver would only defer
			why = whyLoopLocked
			return nil
		}
		return paths.AppendLineRepair(budgetFile(), data)
	})
	switch {
	case readErr != nil:
		return "", fmt.Errorf("%w (inspect budget.jsonl)", readErr)
	case lockErr != nil: // the lock error names a path; stderr here never does
		return "", errors.New("budget ledger lock or write failed (inspect budget.jsonl)")
	}
	return why, nil
}

// reservationRefusal is why sha's reservation cannot start a loop now: absent, already started, another day, or tokens.
func reservationRefusal(rows []budgetRow, sha string, now time.Time) string {
	if why := tokenStop(rows, now); why != "" {
		return why
	}
	var made *budgetRow
	for i, row := range rows {
		switch {
		case row.Kind != KindBenchLoop || row.ID != sha || row.Dispatch != "":
		case row.Started:
			return "launch reservation for " + sha + " already used"
		case made == nil:
			made = &rows[i]
		}
	}
	switch {
	case made == nil:
		return "no launch reservation for " + sha
	case made.At.In(now.Location()).Format(time.DateOnly) != now.Format(time.DateOnly):
		return "launch reservation for " + sha + " is from another day"
	}
	return ""
}

// dayUse is the local day's distinct kind run ids and its token total across every kind.
func dayUse(rows []budgetRow, kind string, now time.Time) (runs, tokens int) {
	ids := map[string]bool{}
	dispatches := map[string]int{}
	for _, row := range rows {
		if row.At.In(now.Location()).Format(time.DateOnly) != now.Format(time.DateOnly) {
			continue
		}
		if row.Started {
			continue
		}
		if row.Dispatch != "" {
			dispatches[row.Dispatch] = max(dispatches[row.Dispatch], row.Tokens)
			continue
		}
		tokens += row.Tokens
		if row.Kind == kind && row.ID != "" {
			ids[row.ID] = true
		}
	}
	for _, t := range dispatches {
		tokens += t
	}
	return len(ids), tokens
}

// Reserve checks the limits (and a held loop lock) and appends id's launch row under one ledger lock: atomic.
func Reserve(kind, id string, now time.Time) (ok bool, why string) {
	row := budgetRow{At: now, Kind: kind, ID: id}
	if id == "" || !validBudgetRow(row) {
		return false, "invalid budget reservation"
	}
	data, err := json.Marshal(row)
	if err != nil {
		return false, "encode budget row"
	}
	err = paths.WithFileLock(budgetFile(), func() error {
		rows, err := readBudget()
		if err != nil {
			why = err.Error()
			return nil
		}
		if why = budgetRefusal(rows, kind, id, now); why != "" {
			return nil
		}
		if kind == KindBenchLoop && loopLocked() {
			why = whyLoopLocked
			return nil
		}
		// AppendLine would take the lock this function already holds.
		return paths.AppendLineRepair(budgetFile(), data)
	})
	if err != nil { // the reason reaches the worker's marker, which never carries a path
		return false, "budget record failed"
	}
	return why == "", why
}

func appendBudgetRow(row budgetRow) error {
	if !validBudgetRow(row) {
		return errors.New("invalid budget row")
	}
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode budget row: %w", err)
	}
	return paths.AppendLine(budgetFile(), data)
}

// settleDispatch waits briefly for a running dispatch to settle through `agents wait`; best effort.
var settleDispatch = func(id string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), settleWait+time.Second)
	defer cancel()
	_, _, _ = execRun(ctx, "", "", exe, "agents", "wait", id, "--timeout", settleWait.String())
}

const settleWait = 2 * time.Second

// readDispatchSpend is a dispatch record's total tokens and whether it still runs.
func readDispatchSpend(id string) (tokens int, running bool, err error) {
	if id != filepath.Base(id) || strings.Contains(id, "..") {
		return 0, false, fmt.Errorf("budget spend %q: not a dispatch id", id)
	}
	b, err := os.ReadFile(filepath.Join(paths.DispatchDir(), id+".json"))
	if err != nil {
		return 0, false, fmt.Errorf("budget spend: %w", err)
	}
	var r struct {
		dispatchRecord
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, false, fmt.Errorf("budget spend %s: %w", id, err)
	}
	if u := r.Usage; u != nil {
		tokens = u.In + u.Out + u.CacheWrite + u.CacheRead
	}
	return tokens, r.Status == "running", nil
}

// SpendDispatch records a dispatch's tokens against kind: its final ones once it settles, else what is known so far.
func SpendDispatch(kind, dispatchID string, now time.Time) error {
	if dispatchID == "" {
		return errors.New("budget spend requires a dispatch id")
	}
	tokens, running, err := readDispatchSpend(dispatchID)
	if err == nil && running {
		settleDispatch(dispatchID)
		tokens, _, err = readDispatchSpend(dispatchID)
	}
	if err != nil {
		return err
	}
	return appendBudgetRow(budgetRow{At: now, Kind: kind, Dispatch: dispatchID, Tokens: tokens})
}

// GateFixBudget is the budget check autofix.FixLeaf takes.
func GateFixBudget() (bool, string) { return allow(KindGateFix, budgetNow()) }

// loopLocked probes runDriver's loop.lock without waiting or creating it; an open failure counts as held.
func loopLocked() bool {
	f, err := os.Open(filepath.Join(benchRoot(), "loop.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	locked := !paths.TryLock(f)
	return f.Close() != nil || locked
}
