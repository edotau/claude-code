package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/source"
)

type pendingRow struct {
	At     time.Time `json:"at"`
	Lane   string    `json:"lane"`
	Pkg    string    `json:"pkg"`
	Sha    string    `json:"sha"`
	Status string    `json:"status"`
	Body   string    `json:"body"`
	Orphan bool      `json:"orphaned,omitempty"` // listing only: the lane branch is gone and no branch holds sha
}

type pendingState struct {
	Rows []pendingRow `json:"rows"`
}

func pendingFile() string { return filepath.Join(benchRoot(), "pending.json") }

func readPending() (pendingState, error) {
	var state pendingState
	body, err := os.ReadFile(pendingFile())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return state, fmt.Errorf("read pending baselines: %w", err)
	}
	if state.Rows == nil {
		return state, errors.New("pending baselines missing rows")
	}
	for _, row := range state.Rows {
		if !validPendingRow(row) {
			return state, errors.New("invalid pending baseline row")
		}
	}
	return state, nil
}

func validPendingRow(row pendingRow) bool {
	return paths.SafeName(row.Lane) && filepath.IsLocal(row.Pkg) && !strings.Contains(row.Pkg, "\\") &&
		!row.At.IsZero() && slices.Contains([]string{"pending", "applied", "dropped"}, row.Status)
}

func writePending(state pendingState) error {
	return paths.WriteJSON(pendingFile(), state)
}

func recordPending(l loopOpts, at time.Time) error {
	if !paths.SafeName(l.lane) {
		return errors.New("a kept --after needs a --lane slug")
	}
	body, err := os.ReadFile(l.after)
	if err != nil {
		return err
	}
	if _, err := pendingEpochs(string(body)); err != nil {
		return err
	}
	return paths.WithFileLockWithin(pendingFile(), rollingLock, func() error {
		state, err := readPending()
		if err != nil {
			return err
		}
		state.Rows = append(state.Rows, pendingRow{At: at, Lane: l.lane, Pkg: l.pkg,
			Sha: l.sha, Status: "pending", Body: string(body)})
		return writePending(state)
	})
}

func pendingEpochs(body string) (map[string]benchmarkEpoch, error) {
	epochs, err := parseEpochs(body)
	if err != nil {
		return nil, err
	}
	names, _ := parseSamples(body)
	if len(epochs) == 0 || len(names) == 0 {
		return nil, errors.New("pending baseline needs benchmark samples and epochs")
	}
	for _, name := range names {
		if _, ok := epochs[benchmarkFunc(name)]; !ok {
			return nil, fmt.Errorf("pending baseline has no epoch for %s", name)
		}
	}
	return epochs, nil
}

// ApplyPending applies a landed lane's baselines against source in top; repeat calls leave completed rows alone.
func ApplyPending(top, lane string) error {
	if !filepath.IsAbs(top) || !paths.SafeName(lane) {
		return errors.New("apply pending needs an absolute repository root and lane slug")
	}
	return paths.WithFileLockWithin(pendingFile(), rollingLock, func() error {
		return applyLanePending(top, lane)
	})
}

func applyLanePending(top, lane string) error {
	state, err := readPending()
	if err != nil {
		return err
	}
	for i, row := range state.Rows {
		if row.Lane != lane || row.Status != "pending" {
			continue
		}
		status, err := applyPendingRow(top, row)
		if err != nil {
			return fmt.Errorf("apply pending %s %s: %w", lane, row.Pkg, err)
		}
		state.Rows[i].Status = status
		if err := writePending(state); err != nil {
			return err
		}
	}
	return nil
}

func applyPendingRow(top string, row pendingRow) (string, error) {
	recorded, err := pendingEpochs(row.Body)
	if err != nil {
		return "", err
	}
	current, err := benchmarkEpochs(top, row.Pkg)
	if err != nil {
		return "", err
	}
	if !maps.Equal(recorded, current) {
		return "dropped", nil
	}
	if err := writeRolling(filepath.Join(benchRoot(), pkgDirName(row.Pkg)), []byte(row.Body)); err != nil {
		return "", err
	}
	return "applied", nil
}

func runPending(l loopOpts, asJSON bool, w io.Writer) error {
	rows, err := pendingRows(l)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(w).Encode(rows)
	}
	for _, row := range rows {
		label, why := "PENDING", ""
		if row.Orphan {
			label, why = "ORPHANED", " (lane branch gone, sha on no branch: can never apply)"
		}
		if _, err := fmt.Fprintf(w, "%s %s %s %s%s\n", label, row.Lane, row.Pkg, row.Sha, why); err != nil {
			return err
		}
	}
	return nil
}

// orphaned is a row whose lane branch is gone and whose sha no branch contains; outside a repo nothing is flagged.
func orphaned(top string, row pendingRow) bool {
	if top == "" {
		return false
	}
	if _, err := source.GitInherit(top, "rev-parse", "--verify", "--quiet", "refs/heads/worktree-"+row.Lane); err == nil {
		return false
	}
	out, err := source.GitInherit(top, "for-each-ref", "--count=1", "--format=%(refname)", "--contains="+row.Sha,
		"refs/heads")
	return err != nil || out == ""
}

func pendingRows(l loopOpts) ([]pendingRow, error) {
	if l.decide || l.queue || l.verdicts || l.verdict+l.after != "" {
		return nil, errors.New("--pending cannot be combined with other loop modes or --after")
	}
	state, err := readPending()
	if err != nil {
		return nil, err
	}
	top, _ := repoTop()
	rows := []pendingRow{}
	for _, row := range state.Rows {
		if row.matches(l) {
			row.Orphan = orphaned(top, row)
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (row pendingRow) matches(l loopOpts) bool {
	return row.Status == "pending" && (l.lane == "" || row.Lane == l.lane) && (l.pkg == "" || row.Pkg == l.pkg)
}
