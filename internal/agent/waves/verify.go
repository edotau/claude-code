// Verify side of a wave: the per-package owned-file check, the per-leaf routing proof and the gate runner.

package waves

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/plans"
)

// Exit codes of CheckUnion, the same as `agents diff`.
const (
	CheckExitGate    = agent.DiffExitGate
	CheckExitOutside = agent.DiffExitOutside
)

// GateTailLines is how many trailing output lines GateRun returns.
const GateTailLines = 40

// CheckEnv holds the side-effecting calls; a nil field means the real one.
type CheckEnv struct {
	// Diff is agent.Diff.
	Diff func(agent.DiffOptions) int
	// Fingerprint is a file's content hash, "absent" when missing, as state.go FingerprintAbsent.
	Fingerprint func(dir, rel string) (string, error)
	// Exec runs script with `bash -c` in dir with extra env, writes combined output to out and returns the exit code.
	Exec func(ctx context.Context, dir, script string, env []string, out io.Writer) (int, error)
	// ModTime is a file's last write time, zero when the file is missing.
	ModTime func(dir, rel string) (time.Time, error)
}

// CheckPackage is one package of the wave; Dispatch is the leaf's dispatch id, whose run window dates its writes.
type CheckPackage struct {
	ID, Dispatch string
	Owns         []string
}

// CheckUnionInput is one wave's owned-file check; Dir is the repo root.
type CheckUnionInput struct {
	Dir, Base string
	Packages  []CheckPackage
	// Launch maps each path that was dirty at launch to its fingerprint then; a clean path is absent.
	Launch map[string]string
}

// CheckViolation is a package that wrote File while only Owners own it.
type CheckViolation struct {
	Package, File string
	Owners        []string
}

// CheckUnionResult is the verdict: Exit 0, CheckExitGate (a gate failed) or CheckExitOutside.
type CheckUnionResult struct {
	Exit         int
	Outside      []string // changed outside every package since launch; pre-existing unchanged dirt is not listed
	Violations   []CheckViolation
	Unattributed []string // changed files owned by another package whose writer the run windows cannot tell
	Gates        []agent.GateResult
}

// checkSlack covers the sub-second part `agents diff --verify` drops from a dispatch's end time.
const checkSlack = time.Second

// CheckUnion runs `agents diff --check` over the union of the packages and then attributes each changed file
// to the package that wrote it; a file with no attributable writer is reported, never passed.
func CheckUnion(env CheckEnv, in CheckUnionInput) (CheckUnionResult, error) {
	owned, err := checkOwned(in.Packages)
	if err != nil {
		return CheckUnionResult{}, err
	}
	rep, err := checkDiff(env, agent.DiffOptions{Dir: in.Dir, Base: in.Base, Paths: checkScope(owned), Check: true})
	if err != nil {
		return CheckUnionResult{}, err
	}
	res := CheckUnionResult{Gates: rep.Gates}
	if res.Outside, err = checkFilter(env, in, rep.Outside); err != nil {
		return CheckUnionResult{}, err
	}
	pkgs, err := checkPackages(env, in)
	if err != nil {
		return CheckUnionResult{}, err
	}
	if res.Violations, res.Unattributed, err = checkAttribute(env, in, pkgs); err != nil {
		return CheckUnionResult{}, err
	}
	switch {
	case slices.ContainsFunc(rep.Gates, func(g agent.GateResult) bool { return g.Status == "FAIL" }):
		res.Exit = CheckExitGate
	case len(res.Outside)+len(res.Violations)+len(res.Unattributed) > 0:
		res.Exit = CheckExitOutside
	}
	return res, nil
}

// checkOwned is the deduplicated union of every package's owns; a package that owns nothing is an error.
func checkOwned(pkgs []CheckPackage) ([]string, error) {
	var owned []string
	for _, p := range pkgs {
		if len(p.Owns) == 0 {
			return nil, fmt.Errorf("check union: package %q owns no paths", p.ID)
		}
		for _, o := range p.Owns {
			if !slices.Contains(owned, o) {
				owned = append(owned, o)
			}
		}
	}
	if len(owned) == 0 {
		return nil, errors.New("check union: no owned paths")
	}
	return owned, nil
}

// checkFilter keeps the files that changed since launch.
func checkFilter(env CheckEnv, in CheckUnionInput, files []string) ([]string, error) {
	var out []string
	for _, p := range files {
		changed, err := checkChanged(env, in, p)
		if err != nil {
			return nil, err
		}
		if changed {
			out = append(out, p)
		}
	}
	return out, nil
}

// checkPkg is a package with the files outside its own scope and the run window of its dispatch (zero: unknown).
type checkPkg struct {
	CheckPackage
	outside    map[string]bool
	start, end time.Time
}

func (p checkPkg) covers(t time.Time) bool {
	return !p.start.IsZero() && !t.Before(p.start) && t.Before(p.end.Add(checkSlack))
}

// checkPackages diffs each package against its own scope; a file outside it is another package's or no one's.
func checkPackages(env CheckEnv, in CheckUnionInput) ([]checkPkg, error) {
	out := make([]checkPkg, 0, len(in.Packages))
	for _, p := range in.Packages {
		o := agent.DiffOptions{Dir: in.Dir, Base: in.Base, Paths: checkScope(p.Owns), Verify: p.Dispatch}
		rep, err := checkDiff(env, o)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", p.ID, err)
		}
		cp := checkPkg{CheckPackage: p, outside: make(map[string]bool, len(rep.Outside))}
		for _, f := range rep.Outside {
			cp.outside[f] = true
		}
		if v := rep.Verify; v != nil {
			s, serr := time.Parse(time.RFC3339, v.Start)
			e, eerr := time.Parse(time.RFC3339, v.End)
			if serr == nil && eerr == nil {
				cp.start, cp.end = s, e
			}
		}
		out = append(out, cp)
	}
	return out, nil
}

// checkAttribute finds, for each changed file owned by some package, who wrote it: the file's last write time
// falling in an owner's run window is the owner's; else the non-owners running then are the violators.
func checkAttribute(env CheckEnv, in CheckUnionInput, pkgs []checkPkg) ([]CheckViolation, []string, error) {
	files := map[string]bool{}
	for _, p := range pkgs {
		for f := range p.outside {
			files[f] = true
		}
	}
	var viol []CheckViolation
	var unattr []string
	for _, f := range slices.Sorted(maps.Keys(files)) {
		v, ok, err := checkFile(env, in, pkgs, f)
		if err != nil {
			return nil, nil, err
		}
		viol = append(viol, v...)
		if !ok {
			unattr = append(unattr, f)
		}
	}
	return viol, unattr, nil
}

// checkFile is the violations of one file; ok is false when it changed, has an owner and its writer is unknown.
func checkFile(env CheckEnv, in CheckUnionInput, pkgs []checkPkg, f string) ([]CheckViolation, bool, error) {
	var owners, others []checkPkg
	for _, p := range pkgs {
		if p.outside[f] {
			others = append(others, p)
		} else {
			owners = append(owners, p)
		}
	}
	if len(owners) == 0 {
		return nil, true, nil
	}
	changed, err := checkChanged(env, in, f)
	if err != nil || !changed {
		return nil, true, err
	}
	mt, err := checkModTime(env, in.Dir, f)
	if err != nil {
		return nil, false, err
	}
	culprits, ok := checkWriters(owners, others, mt)
	var viol []CheckViolation
	for _, c := range culprits {
		viol = append(viol, CheckViolation{Package: c.ID, File: f, Owners: checkIDs(owners)})
	}
	return viol, ok, nil
}

// checkWriters is the non-owners that ran when f was last written; ok is false when nobody can be named.
func checkWriters(owners, others []checkPkg, mt time.Time) (culprits []checkPkg, ok bool) {
	switch {
	case mt.IsZero():
		return nil, false
	case slices.ContainsFunc(owners, func(p checkPkg) bool { return p.covers(mt) }):
		return nil, true
	case slices.ContainsFunc(owners, func(p checkPkg) bool { return p.start.IsZero() }):
		return nil, false
	}
	culprits = slices.DeleteFunc(slices.Clone(others), func(p checkPkg) bool { return !p.covers(mt) })
	return culprits, len(culprits) > 0
}

func checkIDs(pkgs []checkPkg) []string {
	ids := make([]string, len(pkgs))
	for i, p := range pkgs {
		ids[i] = p.ID
	}
	return ids
}

// checkModTime is the last write time of dir/rel, zero when the file is gone.
func checkModTime(env CheckEnv, dir, rel string) (time.Time, error) {
	if env.ModTime != nil {
		return env.ModTime(dir, rel)
	}
	fi, err := os.Lstat(filepath.Join(dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// checkScope adds the _test.go of each owned Go source, which a package owns too.
func checkScope(owned []string) []string {
	sel := slices.Clone(owned)
	for _, p := range owned {
		if src, ok := strings.CutSuffix(p, ".go"); ok && !strings.HasSuffix(src, "_test") {
			sel = append(sel, src+"_test.go")
		}
	}
	return sel
}

// checkChanged is false only for a file that was dirty at launch and still has that fingerprint.
func checkChanged(env CheckEnv, in CheckUnionInput, rel string) (bool, error) {
	was, ok := in.Launch[rel]
	if !ok {
		return true, nil
	}
	fp := env.Fingerprint
	if fp == nil {
		fp = checkHashObject
	}
	now, err := fp(in.Dir, rel)
	if err != nil {
		return false, fmt.Errorf("fingerprint %s: %w", rel, err)
	}
	return now != was, nil
}

// checkHashObject is `git hash-object` of dir/rel, "absent" when the file is gone.
func checkHashObject(dir, rel string) (string, error) {
	if _, err := os.Lstat(filepath.Join(dir, rel)); errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "hash-object", "--", rel).Output()
	return strings.TrimSpace(string(out)), err
}

// checkDiff runs Diff in JSON mode and decodes its report; exit 2 (usage) is an error, 1 and 3 are in the report.
func checkDiff(env CheckEnv, o agent.DiffOptions) (agent.DiffReport, error) {
	run := env.Diff
	if run == nil {
		run = agent.Diff
	}
	var out, errb bytes.Buffer
	o.JSON, o.Out, o.Err = true, &out, &errb
	exit := run(o)
	var rep agent.DiffReport
	if exit == agent.DiffExitUsage {
		return rep, fmt.Errorf("agents diff: %s", strings.TrimSpace(errb.String()))
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		return rep, fmt.Errorf("agents diff exit %d: unreadable report: %v %s", exit, err, strings.TrimSpace(errb.String()))
	}
	return rep, nil
}

// RoutingResult is `agents diff --verify` for one dispatch; Mismatch: agent, model or route differs from the ask.
type RoutingResult struct {
	ID, Agent, Model, Route string
	Mismatch                bool
	Mismatches              []string
}

// RoutingProof reads only verify.mismatch of dispatch id.
func RoutingProof(env CheckEnv, dir, id string) (RoutingResult, error) {
	rep, err := checkDiff(env, agent.DiffOptions{Dir: dir, Verify: id})
	if err != nil {
		return RoutingResult{}, err
	}
	v := rep.Verify
	if v == nil {
		return RoutingResult{}, fmt.Errorf("routing proof %s: report has no verify section", id)
	}
	return RoutingResult{ID: v.ID, Agent: v.Agent, Model: v.Model, Route: v.Route, Mismatch: v.Mismatch,
		Mismatches: v.Mismatches}, nil
}

// GateRefusal is non-nil for a command that would write to git; it is plans.GateRefused, the one refusal the reader uses.
func GateRefusal(cmd string) error { return plans.GateRefused(cmd) }

// GateInput is one command to run: a task's verify or the wave gate. Env is extra KEY=VALUE pairs (BASE, OWNED).
type GateInput struct {
	Dir, Cmd, OutPath string
	Env               []string
}

// GateResult is the exit code, the last GateTailLines lines, and the path holding the full output.
type GateResult struct {
	Exit  int
	Tail  []string
	Saved string
}

// GateRun runs the command as `bash -c` after GateRefusal and saves the full output to in.OutPath.
func GateRun(ctx context.Context, env CheckEnv, in GateInput) (GateResult, error) {
	if err := GateRefusal(in.Cmd); err != nil {
		return GateResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(in.OutPath), 0o755); err != nil {
		return GateResult{}, err
	}
	f, err := os.Create(in.OutPath)
	if err != nil {
		return GateResult{}, err
	}
	tail := &gateTail{max: GateTailLines}
	run := env.Exec
	if run == nil {
		run = gateBash
	}
	exit, err := run(ctx, in.Dir, in.Cmd, in.Env, io.MultiWriter(f, tail))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return GateResult{Exit: exit, Tail: tail.lines(), Saved: in.OutPath}, err
}

func gateBash(ctx context.Context, dir, script string, env []string, out io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, append(os.Environ(), env...), out, out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// gateTail keeps the last max lines written to it.
type gateTail struct {
	max  int
	buf  []string
	part string
}

func (t *gateTail) Write(p []byte) (int, error) {
	t.part += string(p)
	parts := strings.Split(t.part, "\n")
	t.part = parts[len(parts)-1]
	t.buf = append(t.buf, parts[:len(parts)-1]...)
	if n := len(t.buf) - t.max; n > 0 {
		t.buf = slices.Delete(t.buf, 0, n)
	}
	return len(p), nil
}

func (t *gateTail) lines() []string {
	out := slices.Clone(t.buf)
	if t.part != "" {
		out = append(out, t.part)
	}
	if n := len(out) - t.max; n > 0 {
		out = out[n:]
	}
	return out
}
