package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/bench"
)

const dreamRotationSize = 2

type dreamBenchState struct {
	RankedAt time.Time `json:"ranked_at"`
	Ranking  []string  `json:"ranking"`
	Last     string    `json:"last"`
}

type dreamGoPackage struct {
	Dir          string
	ImportPath   string
	Imports      []string
	TestGoFiles  []string
	XTestGoFiles []string
}

func dreamBenchRotation(out io.Writer) error {
	root := paths.ConfigDir()
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("skipped: harness source: %w", err)
	}
	dir := filepath.Join(paths.StateDir(), "bench")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ran, err := paths.TryFileLock(filepath.Join(dir, "rotation.lock"), 0, func() error {
		return rotateDreamBench(root, filepath.Join(dir, "rotation.json"), out)
	})
	if !ran && err == nil {
		fmt.Fprintln(out, "memory dream: rotation: skipped (already running)")
	}
	return err
}

func rotateDreamBench(root, file string, out io.Writer) error {
	state, err := loadDreamBenchRanking(root, file)
	if err != nil {
		return err
	}
	selected := nextDreamBenchPackages(state.Ranking, state.Last)
	if len(selected) == 0 {
		fmt.Fprintln(out, "memory dream: rotation: skipped (no benchmark packages)")
	}
	var errs []error
	for _, pkg := range selected {
		if err := dreamBenchIdle(); err != nil {
			dreamStepError(out, "rotation: skipped "+pkg, err)
			break
		}
		err := runDreamBench(root, pkg, out)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pkg, err))
		}
		state.Last = pkg
		if err := saveDreamBenchState(file, state); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

func loadDreamBenchRanking(root, file string) (dreamBenchState, error) {
	state, err := readDreamBenchState(file)
	if err != nil {
		return state, err
	}
	if age := time.Since(state.RankedAt); age >= 0 && age < 24*time.Hour {
		return state, nil
	}
	state.Ranking, err = rankDreamBenchPackages(root)
	if err != nil {
		return state, err
	}
	state.RankedAt = time.Now()
	return state, saveDreamBenchState(file, state)
}

func readDreamBenchState(file string) (dreamBenchState, error) {
	var state dreamBenchState
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("rotation state: %w", err)
	}
	for _, pkg := range state.Ranking {
		if !filepath.IsLocal(pkg) || strings.Contains(pkg, `\`) {
			return state, fmt.Errorf("rotation package is not local: %q", pkg)
		}
	}
	return state, nil
}

func saveDreamBenchState(file string, state dreamBenchState) error {
	return paths.WriteJSON(file, state)
}

func nextDreamBenchPackages(ranking []string, last string) []string {
	if len(ranking) == 0 {
		return nil
	}
	start := (slices.Index(ranking, last) + 1) % len(ranking)
	selected := make([]string, 0, min(dreamRotationSize, len(ranking)))
	for i := range cap(selected) {
		selected = append(selected, ranking[(start+i)%len(ranking)])
	}
	return selected
}

func rankDreamBenchPackages(root string) ([]string, error) {
	pkgs, err := listDreamBenchPackages(root)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, pkg := range pkgs {
		for _, imported := range pkg.Imports {
			counts[imported]++
		}
	}
	slices.SortFunc(pkgs, func(a, b dreamGoPackage) int {
		if n := cmp.Compare(counts[b.ImportPath], counts[a.ImportPath]); n != 0 {
			return n
		}
		return cmp.Compare(a.ImportPath, b.ImportPath)
	})
	var ranking []string
	for _, pkg := range pkgs {
		has, err := dreamPackageHasBench(pkg)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		rel, err := filepath.Rel(root, pkg.Dir)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("go list directory outside harness: %s", pkg.Dir)
		}
		ranking = append(ranking, filepath.ToSlash(rel))
	}
	return ranking, nil
}

func listDreamBenchPackages(root string) ([]dreamGoPackage, error) {
	data, err := dreamBenchCommand(root, time.Minute, "go", "list", "-json", "./...")
	if err != nil {
		return nil, fmt.Errorf("importer ranking: %w", err)
	}
	var pkgs []dreamGoPackage
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg dreamGoPackage
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("go list JSON: %w", err)
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func dreamPackageHasBench(pkg dreamGoPackage) (bool, error) {
	files := append(slices.Clone(pkg.TestGoFiles), pkg.XTestGoFiles...)
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && dreamBenchmarkName(fn.Name.Name) {
				return true, nil
			}
		}
	}
	return false, nil
}

func dreamBenchmarkName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Benchmark")
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

func dreamBenchIdle() error {
	var data []byte
	var err error
	switch runtime.GOOS {
	case "linux":
		data, err = os.ReadFile("/proc/loadavg")
	case "darwin":
		data, err = dreamBenchCommand("", 5*time.Second, "sysctl", "-n", "vm.loadavg")
	default:
		return fmt.Errorf("load average unavailable on %s", runtime.GOOS)
	}
	if err != nil {
		return fmt.Errorf("load average unavailable: %w", err)
	}
	fields := strings.Fields(strings.Trim(string(data), "{} \n\t"))
	if len(fields) == 0 {
		return errors.New("load average is empty")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || !(load >= 0 && load < float64(runtime.NumCPU())/2) {
		return fmt.Errorf("load %s is not below %.1f", fields[0], float64(runtime.NumCPU())/2)
	}
	return nil
}

func dreamBenchCommand(root string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.WaitDelay = root, time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

func runDreamBench(root, pkg string, out io.Writer) error {
	data, err := dreamBenchCommand(root, 35*time.Minute, "claude-code", "review", "bench", "--path", filepath.Join(root, pkg), "--count", "5", "--format", "json")
	if err != nil {
		return err
	}
	var report bench.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("review bench JSON: %w", err)
	}
	queued := 0
	for _, result := range report.Packages {
		for _, score := range result.Benchmarks {
			if !dreamBenchMoved(score) {
				continue
			}
			entry := dreamBenchEntry{At: time.Now(), Source: "dream-rotation", Module: score.Name, Pkg: result.Pkg, Deltas: score.Deltas}
			if d, ok := score.Deltas["sec/op"]; ok {
				before, e1 := strconv.ParseFloat(d.Old, 64)
				after, e2 := strconv.ParseFloat(d.New, 64)
				if err := errors.Join(e1, e2); err != nil {
					return fmt.Errorf("benchmark duration: %w", err)
				}
				const msPerSecond = float64(time.Second / time.Millisecond)
				entry.BeforeMS, entry.AfterMS = before*msPerSecond, after*msPerSecond
			}
			if err := queueDreamBench(entry); err != nil {
				return err
			}
			queued++
		}
	}
	fmt.Fprintf(out, "memory dream: rotation: %s (%d significant benchmark(s) queued)\n", pkg, queued)
	return nil
}

func dreamBenchMoved(score bench.Score) bool {
	for _, delta := range score.Deltas {
		if delta.Significant {
			return true
		}
	}
	return false
}
