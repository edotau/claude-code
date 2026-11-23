package bench

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	return !row.At.IsZero() && budgetLimit(row.Kind) > 0 && row.Tokens >= 0
}

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

// Allow refuses a run once today's count of kind or today's token total reaches its limit; a bad ledger refuses.
func Allow(kind string, now time.Time) (ok bool, why string) {
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

// dayUse is the local day's distinct kind run ids and its token total across every kind.
func dayUse(rows []budgetRow, kind string, now time.Time) (runs, tokens int) {
	ids := map[string]bool{}
	for _, row := range rows {
		if row.At.In(now.Location()).Format(time.DateOnly) != now.Format(time.DateOnly) {
			continue
		}
		tokens += row.Tokens
		if row.Kind == kind && row.ID != "" {
			ids[row.ID] = true
		}
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
			why = "loop already locked"
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

// Record appends one ledger row; an empty id is spend only, so a run's tokens never count it twice.
func Record(kind, id string, tokens int, now time.Time) error {
	row := budgetRow{At: now, Kind: kind, ID: id, Tokens: tokens}
	if !validBudgetRow(row) {
		return errors.New("invalid budget row")
	}
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode budget row: %w", err)
	}
	return paths.AppendLine(budgetFile(), data)
}

// SpendDispatch records a finished dispatch's input, output and cache tokens against kind.
func SpendDispatch(kind, dispatchID string, now time.Time) error {
	if dispatchID == "" {
		return errors.New("budget spend requires a dispatch id")
	}
	l, err := fromDispatch(loopOpts{dispatch: dispatchID})
	if err != nil {
		return err
	}
	return Record(kind, "", l.tokensIn+l.tokensOut+l.tokensCache, now)
}

// GateFixBudget is the budget check autofix.FixLeaf takes.
func GateFixBudget() (bool, string) { return Allow(KindGateFix, budgetNow()) }

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
