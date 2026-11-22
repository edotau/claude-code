package memory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/bench"
	"github.com/edotau/claude-code/internal/settings"
)

const (
	dreamP95Window  = 7 * 24 * time.Hour
	dreamP95MinRuns = 200
	dreamP95Growth  = 1.2
)

type dreamBenchEntry struct {
	At       time.Time              `json:"at"`
	Source   string                 `json:"source"`
	Module   string                 `json:"module"`
	Pkg      string                 `json:"pkg"`
	BeforeMS float64                `json:"before_ms"`
	AfterMS  float64                `json:"after_ms"`
	Deltas   map[string]bench.Delta `json:"deltas,omitempty"`
}

type dreamHookRow struct {
	T      time.Time `json:"t"`
	Module string    `json:"module"`
	MS     *float64  `json:"ms"`
	Sid    string    `json:"sid"`
	N      int       `json:"n"`
}

type dreamHookWindows struct{ before, after []float64 }

func readDreamP95(dir string, now time.Time) ([]dreamBenchEntry, error) {
	windows := map[string]*dreamHookWindows{}
	start := now.Add(-2 * dreamP95Window)
	var errs []error
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, now.Location())
	for ; !day.After(now); day = day.AddDate(0, 0, 1) {
		file := filepath.Join(dir, "telemetry-"+day.Format("20060102")+".jsonl")
		f, err := os.Open(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = scanDreamP95(f, now, windows)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(file), err))
		}
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	var flags []dreamBenchEntry
	for _, module := range slices.Sorted(maps.Keys(windows)) {
		w := windows[module]
		if len(w.before) < dreamP95MinRuns || len(w.after) < dreamP95MinRuns {
			continue
		}
		before, after := dreamP95(w.before), dreamP95(w.after)
		if after > before*dreamP95Growth {
			flags = append(flags, dreamBenchEntry{At: now, Source: "dream-p95", Module: module,
				Pkg: dreamHookPackage(module), BeforeMS: before, AfterMS: after})
		}
	}
	return flags, errors.Join(errs...)
}

func scanDreamP95(r io.Reader, now time.Time, windows map[string]*dreamHookWindows) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 1<<20)
	invalid, folded := 0, 0
	for scanner.Scan() {
		var row dreamHookRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			invalid++
			continue
		}
		if row.outsideDreamWindow(now) {
			continue
		}
		if row.N > 1 {
			folded++
			continue
		} // Folded durations are maxima, not individual observations.
		if !row.validDreamDuration() {
			invalid++
			continue
		}
		w := windows[row.Module]
		if w == nil {
			w = &dreamHookWindows{}
			windows[row.Module] = w
		}
		if row.T.Before(now.Add(-dreamP95Window)) {
			w.before = append(w.before, *row.MS)
		} else {
			w.after = append(w.after, *row.MS)
		}
	}
	var err error
	if invalid+folded > 0 {
		err = fmt.Errorf("ignored %d invalid and %d folded rows", invalid, folded)
	}
	return errors.Join(err, scanner.Err())
}

func (row dreamHookRow) outsideDreamWindow(now time.Time) bool {
	return row.Sid == "livechec" || row.T.Before(now.Add(-2*dreamP95Window)) || !row.T.Before(now)
}

func (row dreamHookRow) validDreamDuration() bool {
	return row.Module != "" && row.MS != nil && *row.MS >= 0 && !math.IsInf(*row.MS, 0)
}

func dreamP95(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[len(sorted)-1-len(sorted)/20]
}

func dreamHookPackage(module string) string {
	groups := map[string]string{
		"safety": "safety", "safety-edit": "safety", "permission": "safety", "rtk": "safety",
		"docs": "docs", "paths": "docs", "memory": "memhook", "format": "quality", "review": "quality",
		"worktree": "wthook",
	}
	for _, group := range settings.Registry {
		if !slices.ContainsFunc(group.Events, func(s settings.EventSpec) bool { return s.Name == module }) {
			continue
		}
		if module == "docs-plan-reminder" || module == "worktree-busy" {
			return "internal/hooks"
		}
		if sub, ok := groups[group.Name]; ok {
			return "internal/hooks/" + sub
		}
		return "internal/hooks"
	}
	return module
}

func dreamP95Line(flag dreamBenchEntry) string {
	return fmt.Sprintf("hook p95 %s: %.3f -> %.3f ms (>20%%; >=%d runs/window)", flag.Module, flag.BeforeMS, flag.AfterMS, dreamP95MinRuns)
}

func writeDreamP95(out io.Writer, flags []dreamBenchEntry, err error) {
	fmt.Fprintf(out, "memory dream: hook p95: %d regression(s)\n", len(flags))
	for _, flag := range flags {
		fmt.Fprintln(out, "memory dream:", dreamP95Line(flag))
	}
	dreamStepError(out, "hook p95", err)
}

func dreamStepError(out io.Writer, step string, err error) {
	if err != nil {
		fmt.Fprintf(out, "memory dream: %s: %s\n", step, strings.Join(strings.Fields(err.Error()), " "))
	}
}

func dreamPerformance(out io.Writer) []string {
	flags, err := readDreamP95(filepath.Join(paths.StateDir(), "hooks"), time.Now())
	writeDreamP95(out, flags, err)
	var notice []string
	for _, flag := range flags {
		notice = append(notice, dreamP95Line(flag))
		dreamStepError(out, "hook p95 queue", queueDreamBench(flag))
	}
	dreamStepError(out, "rotation", dreamBenchRotation(out))
	return notice
}

func queueDreamBench(entry dreamBenchEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return paths.AppendLine(filepath.Join(paths.StateDir(), "bench", "queue.jsonl"), data)
}
