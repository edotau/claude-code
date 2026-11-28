package bench

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const statusMSPerSecond = float64(time.Second / time.Millisecond)

// Status is `review bench --status`: today's budget, automatic lanes, the queue and the loop.
type Status struct {
	At              time.Time     `json:"at"`
	AuditMode       string        `json:"audit_mode,omitempty"`
	AuditCandidates []Candidate   `json:"audit_candidates,omitempty"`
	Budget          *StatusBudget `json:"budget,omitempty"`
	BudgetError     string        `json:"budget_error,omitempty"`
	Lanes           []StatusLane  `json:"lanes"`
	LaneCap         int           `json:"lane_cap"`
	LanesError      string        `json:"lanes_error,omitempty"`
	Queue           *StatusQueue  `json:"queue,omitempty"`
	QueueError      string        `json:"queue_error,omitempty"`
	LoopRunning     bool          `json:"loop_running"`
	LastLoop        *StatusLaunch `json:"last_loop,omitempty"`
	LoopError       string        `json:"loop_error,omitempty"`
}

type StatusBudget struct {
	Tokens     int                `json:"tokens"`
	TokenLimit int                `json:"token_limit"`
	Kinds      []StatusBudgetKind `json:"kinds"`
}

type StatusBudgetKind struct {
	Kind    string `json:"kind"`
	Runs    int    `json:"runs"`
	Limit   int    `json:"limit"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type StatusLane struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Merged bool   `json:"merged"`
	Dirty  bool   `json:"dirty"`
	Behind int    `json:"behind"`
}

type StatusQueue struct {
	Total       int            `json:"total"`
	Counts      map[string]int `json:"counts"`
	Top         []Item         `json:"top"`
	FloorMSDay  float64        `json:"floor_ms_day"`
	ClearsFloor bool           `json:"clears_floor"`
}

type StatusLaunch struct {
	End        time.Time `json:"end"`
	Reason     string    `json:"reason"`
	AgeSeconds float64   `json:"age_seconds"`
}

// BuildStatus reads every section without writing or locking; a failed section carries its error.
func BuildStatus(ctx context.Context, top string, now time.Time) Status {
	cands, _ := PendingCandidates()
	s := Status{At: now, AuditMode: AuditMode(), AuditCandidates: cands, LaneCap: maxAutoLanes, Lanes: []StatusLane{}, LoopRunning: loopLocked()}
	rows, err := readBudget()
	if err != nil {
		s.BudgetError = err.Error()
	} else {
		_, tokens := dayUse(rows, KindBenchLoop, now)
		s.Budget = &StatusBudget{Tokens: tokens, TokenLimit: dailyTokens}
		for _, kind := range []string{KindBenchLoop, KindGateFix} {
			runs, _ := dayUse(rows, kind, now)
			why := budgetRefusal(rows, kind, "", now)
			s.Budget.Kinds = append(s.Budget.Kinds, StatusBudgetKind{kind, runs, budgetLimit(kind), why == "", why})
		}
	}
	lanes, err := autoLanes(ctx, top)
	if err != nil {
		s.LanesError = err.Error()
	}
	for _, lane := range lanes {
		s.Lanes = append(s.Lanes, StatusLane{lane.Name, lane.Branch, lane.Merged, lane.Dirty, lane.Behind})
	}
	s.Queue, err = statusQueue()
	if err != nil {
		s.QueueError = err.Error()
	}
	s.LastLoop, err = statusLastLoop(now)
	if err != nil {
		s.LoopError = err.Error()
	}
	return s
}

func statusQueue() (*StatusQueue, error) {
	// collect seeds a missing candidate ledger, so status must refuse that side effect.
	if _, err := os.Stat(filepath.Join(benchRoot(), "candidates.jsonl")); err != nil {
		return nil, errors.New("not seeded; `claude-code review bench --queue` seeds it")
	}
	var buf bytes.Buffer
	if err := runQueue(loopOpts{}, true, &buf); err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		return nil, fmt.Errorf("decode queue: %w", err)
	}
	q := &StatusQueue{Total: len(items), Counts: map[string]int{}, FloorMSDay: minScoreMSDay}
	for _, item := range items {
		q.Counts[item.Kind]++
		// the driver's pick only takes fix items, so a weak item's score never starts a loop
		q.ClearsFloor = q.ClearsFloor || item.Kind == "fix" && item.score() >= minScoreMSDay
	}
	q.Top = items[:min(3, len(items))]
	return q, nil
}

func statusLastLoop(now time.Time) (*StatusLaunch, error) {
	files, err := filepath.Glob(filepath.Join(benchRoot(), "post-commit", "*.marker"))
	if err != nil {
		return nil, errors.New("cannot list launch decisions")
	}
	var last *StatusLaunch
	for _, file := range files {
		end, reason, err := readLaunch(file)
		if err != nil {
			return nil, err
		}
		if reason != "" && (last == nil || end.After(last.End)) {
			last = &StatusLaunch{end, reason, max(0, now.Sub(end).Seconds())}
		}
	}
	return last, nil
}

// readLaunch is one marker's loop decision and its time; no decision is an empty reason.
func readLaunch(file string) (time.Time, string, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) { // the worker prunes markers while we list them
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", errors.New("cannot read launch decision")
	}
	var marker struct {
		End  time.Time `json:"end"`
		Loop string    `json:"loop"`
	}
	if json.Unmarshal(data, &marker) != nil {
		return time.Time{}, "", nil
	}
	if marker.Loop == "" || !marker.End.IsZero() {
		return marker.End, marker.Loop, nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return time.Time{}, "", errors.New("cannot date launch decision")
	}
	return info.ModTime(), marker.Loop, nil
}

func (s Status) writeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func (s Status) writeText(w io.Writer) error {
	rows := s.budgetText()
	rows = append(rows, s.lanesText()...)
	rows = append(rows, s.queueText()...)
	if s.AuditMode != "" {
		rows = append(rows, fmt.Sprintf("audit mode: %s (%d pending)", s.AuditMode, len(s.AuditCandidates)))
	}
	state := "idle"
	if s.LoopRunning {
		state = "running"
	}
	rows = append(rows, "loop: "+state)
	if s.LastLoop != nil {
		age := (time.Duration(s.LastLoop.AgeSeconds) * time.Second).String()
		rows = append(rows, fmt.Sprintf("last launch: %s ago; %s", age, s.LastLoop.Reason))
	}
	if s.LoopError != "" {
		rows = append(rows, "last launch: "+s.LoopError)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(w, statusTextLine(row)); err != nil {
			return err
		}
	}
	return nil
}

func (s Status) budgetText() []string {
	if s.BudgetError != "" {
		return []string{"budget: " + s.BudgetError}
	}
	if s.Budget == nil {
		return []string{"budget: unavailable"}
	}
	rows := []string{fmt.Sprintf("tokens: %d/%d today", s.Budget.Tokens, s.Budget.TokenLimit)}
	for _, kind := range s.Budget.Kinds {
		rows = append(rows, fmt.Sprintf("%s: %d/%d today", kind.Kind, kind.Runs, kind.Limit))
		decision := "allowed"
		if !kind.Allowed {
			decision = "refused: " + kind.Reason
		}
		rows = append(rows, kind.Kind+" launch: "+decision)
	}
	return rows
}

func (s Status) lanesText() []string {
	if s.LanesError != "" {
		return []string{"lanes: " + s.LanesError}
	}
	rows := []string{fmt.Sprintf("lanes: %d open (cap %d)", len(s.Lanes), s.LaneCap)}
	for _, lane := range s.Lanes {
		state := "unmerged"
		if lane.Merged {
			state = "merged"
		}
		rows = append(rows, fmt.Sprintf("%s: %s, dirty=%t, behind=%d", lane.Name, state, lane.Dirty, lane.Behind))
		if !lane.Merged && !lane.Dirty {
			rows = append(rows, "claude-code worktree land "+lane.Name)
		}
	}
	return rows
}

func (s Status) queueText() []string {
	if s.QueueError != "" {
		return []string{"queue: " + s.QueueError}
	}
	if s.Queue == nil {
		return []string{"queue: unavailable"}
	}
	q := s.Queue
	rows := []string{fmt.Sprintf("queue: %d items", q.Total)}
	kinds := make([]string, 0, len(q.Counts))
	for kind := range q.Counts {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		rows = append(rows, fmt.Sprintf("queue %s: %d", kind, q.Counts[kind]))
	}
	for i, item := range q.Top {
		rows = append(rows, fmt.Sprintf("%d. %.2f s/day [%s] %s: %s",
			i+1, item.score()/statusMSPerSecond, item.Kind, item.Pkg, item.What))
	}
	if !q.ClearsFloor {
		rows = append(rows, fmt.Sprintf("no fix item clears the %g s/day loop floor", q.FloorMSDay/statusMSPerSecond))
	}
	return rows
}

func statusTextLine(row string) string {
	row = strings.Join(strings.Fields(row), " ")
	if len(row) <= 80 {
		return row
	}
	end := 0
	for i := range row {
		if i > 77 {
			break
		}
		end = i
	}
	return row[:end] + "..."
}

func runStatus(asJSON bool, w io.Writer) error {
	top, err := repoTop()
	if err != nil {
		return err
	}
	s := BuildStatus(context.Background(), top, time.Now())
	if asJSON {
		return s.writeJSON(w)
	}
	return s.writeText(w)
}

type auditReport struct {
	Mode       string      `json:"mode"`
	Candidates []Candidate `json:"candidates"`
	Pending    []Item      `json:"pending"`
}

func runAudit(arg string, asJSON bool, w io.Writer) error {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg != "status" {
		if err := SetAuditMode(arg); err != nil {
			return err
		}
	}
	cands, err := PendingCandidates()
	if err != nil {
		return err
	}
	var pending []Item
	if q, err := statusQueue(); err == nil && q != nil {
		pending = q.Top
	}
	rep := auditReport{Mode: AuditMode(), Candidates: cands, Pending: pending}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeAuditText(w, rep)
}

func writeAuditText(w io.Writer, rep auditReport) error {
	rows := []string{"audit mode: " + rep.Mode, fmt.Sprintf("candidates: %d pending", len(rep.Candidates))}
	for i, c := range rep.Candidates {
		rows = append(rows, fmt.Sprintf("%d. [%s] %s: %s (%.1f ms vs %.1f ms budget; %s)",
			i+1, c.Kind, c.Area, cmp.Or(c.Detail, c.Metric), c.ObservedMs, c.BudgetMs, c.Metric))
	}
	rows = append(rows, fmt.Sprintf("pending items: %d", len(rep.Pending)))
	for i, it := range rep.Pending {
		rows = append(rows, fmt.Sprintf("%d. [%s] %s: %s", i+1, it.Kind, it.Pkg, it.What))
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(w, statusTextLine(row)); err != nil {
			return err
		}
	}
	return nil
}
