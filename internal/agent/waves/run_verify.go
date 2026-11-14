package waves

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/plans"
)

type stepResult struct {
	Cmd   string   `json:"cmd"`
	Exit  int      `json:"exit"`
	Saved string   `json:"saved"`
	Tail  []string `json:"tail,omitempty"`
}

func (r *runner) fingerprint(path string) (string, error) {
	var paths []string
	for _, pattern := range checkScope([]string{path}) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		paths = append(paths, matches...)
	}
	var err error
	var files []string
	for _, p := range paths {
		err = filepath.WalkDir(p, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	slices.Sort(files)
	hash := sha256.New()
	for _, p := range files {
		fp, err := FingerprintGit(r.o.Dir)(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%s\x00", p, fp)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (r *runner) record(w WaveState, envelopes []Envelope) (WaveState, error) {
	for _, e := range envelopes {
		id := strings.TrimPrefix(e.TaskID, r.slug+"-")
		i := slices.IndexFunc(w.Leaves, func(l LeafState) bool { return l.ID == id })
		if i < 0 {
			return w, fmt.Errorf("unexpected leaf %s", e.TaskID)
		}
		l := w.Leaves[i]
		l.Status, l.DispatchID, l.Saved, l.Verdict, l.Scoped = e.Status, e.DispatchID, e.Report, "pending", e.Scoped
		l.Wrote = relFiles(r.o.Dir, e.Foreign)
		fingerprints, err := FingerprintAll(r.fingerprint, l.Owns)
		if err != nil {
			return w, err
		}
		l.Fingerprints = fingerprints
		w = w.WithLeaf(l)
		if err := r.save(w); err != nil {
			return w, err
		}
	}
	return w, nil
}

// relFiles is vs with each absolute file made relative to dir.
func relFiles(dir string, vs []CheckViolation) []CheckViolation {
	out := slices.Clone(vs)
	for i, v := range out {
		if rel, err := filepath.Rel(dir, v.File); err == nil && filepath.IsAbs(v.File) {
			out[i].File = rel
		}
	}
	return out
}

func hasRunning(w WaveState) bool {
	return slices.ContainsFunc(w.Leaves, func(l LeafState) bool {
		return l.Status == "partial" || l.Status == "stalled"
	})
}

func (r *runner) verify(ctx context.Context, wave plans.Wave, w WaveState, specs []Spec, launchErr error) (int, error) {
	code := 0
	if launchErr != nil {
		code, w.Reason = 1, launchErr.Error()
	}
	var err error
	w, checked, err := r.checkWave(w)
	if err != nil {
		return 1, err
	}
	if checked != 0 {
		code = checked
	}
	for _, s := range specs {
		var err error
		w, err = r.verifyLeaf(ctx, w, s)
		if err != nil {
			return r.verifyError(ctx, w, err)
		}
		l := w.Leaves[slices.IndexFunc(w.Leaves, func(l LeafState) bool { return l.ID == s.ID })]
		if l.Verify != nil && l.Verify.Exit != 0 {
			code = 1
		}
		if leafFailed(l) && code == 0 {
			code = 1
		}
	}
	return r.complete(ctx, wave, w, code)
}

func (r *runner) complete(ctx context.Context, wave plans.Wave, w WaveState, code int) (int, error) {
	if cmd := r.gate(wave); cmd != "" {
		result, err := r.step(ctx, cmd, fmt.Sprintf("wave-%d", w.N), nil)
		w.Verifier = &result
		if err != nil || result.Exit != 0 {
			code, w.Reason = 1, "wave gate failed"
		}
	}
	if ctx.Err() != nil {
		code, w.Status, w.Reason = 4, "stopped", "stopped, same command resumes"
	} else if code == 0 {
		w.Status = WaveStatePassed
	} else {
		w.Status = WaveStateFailed
	}
	return code, r.finish(w)
}

func (r *runner) verifyLeaf(ctx context.Context, w WaveState, s Spec) (WaveState, error) {
	i := slices.IndexFunc(w.Leaves, func(l LeafState) bool { return l.ID == s.ID })
	l := w.Leaves[i]
	if l.Verdict == LeafStateOK {
		return w, nil
	}
	l.Verdict = "failed"
	if l.DispatchID != "" {
		proof, err := RoutingProof(r.h.check, r.o.Dir, l.DispatchID)
		if err != nil {
			return w, err
		}
		w.Proof = append(slices.DeleteFunc(slices.Clone(w.Proof), func(p RoutingResult) bool { return p.ID == proof.ID }), proof)
		l.Mismatch, l.ElapsedMs = proof.Mismatch, proof.DurationMs
		if err = r.save(w.WithLeaf(l)); err != nil {
			return w, err
		}
	}
	passed, result, err := r.taskCheck(ctx, s)
	l.Verify = result
	if err != nil {
		return w, err
	}
	if leafPassed(l, w, passed) {
		l.Verdict = LeafStateOK
	}
	w = w.WithLeaf(l)
	return w, r.save(w)
}

func (r *runner) taskCheck(ctx context.Context, s Spec) (bool, *stepResult, error) {
	if s.Verify == "" {
		rep, err := checkDiff(r.h.check, agent.DiffOptions{Dir: r.o.Dir, Base: r.state.Base, Paths: checkScope(s.Owns), Check: true})
		return !slices.ContainsFunc(rep.Gates, func(g agent.GateResult) bool { return g.Status == "FAIL" }), nil, err
	}
	result, err := r.step(ctx, s.Verify, "verify-"+s.ID, s.Owns)
	return result.Exit == 0, &result, err
}

func (r *runner) step(ctx context.Context, cmd, name string, owns []string) (stepResult, error) {
	result, err := GateRun(ctx, r.h.check, GateInput{Dir: r.o.Dir, Cmd: cmd, OutPath: filepath.Join(r.dir, name+".txt"),
		Env: []string{"BASE=" + r.state.Base, "OWNED=" + strings.Join(owns, "\n")}})
	return stepResult{Cmd: cmd, Exit: result.Exit, Saved: result.Saved, Tail: result.Tail}, err
}

func (r *runner) checkWave(w WaveState) (WaveState, int, error) {
	code := 0
	var packages []CheckPackage
	for _, l := range w.Leaves {
		packages = append(packages, CheckPackage{ID: l.ID, Dispatch: l.DispatchID, Owns: l.Owns, Scoped: l.Scoped})
	}
	if len(packages) > 0 {
		res, err := CheckUnion(r.h.check, CheckUnionInput{Dir: r.o.Dir, Base: r.state.Base, Launch: r.state.Dirty, Packages: packages})
		if err != nil {
			return w, 1, err
		}
		w.Outside, w.Unattributed, w.Gates, w.Violations = res.Outside, res.Unattributed, res.Gates, res.Violations
		w.Fallback = res.Fallback
		for _, l := range w.Leaves {
			w.Violations = append(w.Violations, l.Wrote...)
			if len(l.Wrote) > 0 && res.Exit == 0 {
				res.Exit = CheckExitOutside
			}
		}
		if res.Exit != 0 {
			code = res.Exit
			w.Reason = "owned-file check failed"
		}
		if err = r.save(w); err != nil {
			return w, 1, err
		}
	}
	return w, code, nil
}

func leafPassed(l LeafState, w WaveState, passed bool) bool {
	violated := slices.ContainsFunc(w.Violations, func(v CheckViolation) bool { return v.Package == l.ID })
	return l.Status == LeafStateOK && !l.Mismatch && passed && !violated && len(w.Outside)+len(w.Unattributed) == 0
}

func (r *runner) verifyError(ctx context.Context, w WaveState, err error) (int, error) {
	if ctx.Err() != nil {
		w.Status, w.Reason = "stopped", "stopped, same command resumes"
		return 4, r.finish(w)
	}
	return 1, err
}

func leafFailed(l LeafState) bool { return l.Status != LeafStateOK || l.Verdict != LeafStateOK }
