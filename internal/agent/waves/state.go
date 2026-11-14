// Package waves runs a plan's waves of leaf agents; state.go holds the resume state and the resume decision.
package waves

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// FingerprintAbsent is the fixed fingerprint of a file that does not exist.
const FingerprintAbsent = "absent"

// Wave and leaf status values stored in state.json.
const (
	WaveStatePassed   = "passed"
	WaveStateFailed   = "failed"
	WaveStateRunning  = "running"
	LeafStateOK       = "ok"
	LeafStateMismatch = "mismatch"
)

// FingerprintFunc returns the content fingerprint of path, or FingerprintAbsent when it does not exist.
type FingerprintFunc func(path string) (string, error)

// State is the resume record of one plan run, stored as state.json.
type State struct {
	Plan  string            `json:"plan"`
	Next  string            `json:"next"`
	Base  string            `json:"base"`  // git rev-parse HEAD at launch
	Dirty map[string]string `json:"dirty"` // path -> fingerprint of every file already dirty at launch
	Waves []WaveState       `json:"waves"`
}

// WaveState is one wave's record; Status is a WaveState* constant.
type WaveState struct {
	N          int                `json:"n"`
	Status     string             `json:"status"`
	Leaves     []LeafState        `json:"leaves"`
	Reason     string             `json:"reason,omitempty"`
	Outside    []string           `json:"outside"`
	Gates      []agent.GateResult `json:"gates"`
	Verifier   *stepResult        `json:"verifier,omitempty"`
	Proof      []RoutingResult    `json:"proof"`
	Violations []CheckViolation   `json:"violations,omitempty"`
	Manual     []string           `json:"manual,omitempty"`
	Packages   []Spec             `json:"packages,omitempty"`
}

// LeafState is one leaf's record; Status is the envelope status, Verdict "" or "ok" unless the routing check failed.
type LeafState struct {
	ID           string            `json:"id"`
	Agent        string            `json:"agent"`
	Profile      string            `json:"profile,omitempty"`
	DispatchID   string            `json:"dispatch_id,omitempty"`
	Status       string            `json:"status"`
	Saved        string            `json:"saved,omitempty"`
	Owns         []string          `json:"owns"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"` // Owns -> fingerprint when the leaf ended
	Verify       *stepResult       `json:"verify,omitempty"`
	Mismatch     bool              `json:"mismatch"`
	Verdict      string            `json:"verdict,omitempty"`
}

// FingerprintGit returns a FingerprintFunc hashing files with `git hash-object`; relative paths resolve under dir,
// and any path outside dir and the harness state dir is refused.
func FingerprintGit(dir string) FingerprintFunc {
	return func(path string) (string, error) {
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if path = filepath.Clean(path); !paths.Within(path, dir) && !paths.Within(path, paths.StateDir()) {
			return "", fmt.Errorf("fingerprint %s: outside the repo %s and the state dir", path, dir)
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return FingerprintAbsent, nil
		} else if err != nil {
			return "", err
		}
		return proc.RunGit(context.Background(), dir, "hash-object", "--", path)
	}
}

// FingerprintAll fingerprints every path into a new map.
func FingerprintAll(fp FingerprintFunc, files []string) (map[string]string, error) {
	out := make(map[string]string, len(files))
	for _, f := range files {
		h, err := fp(f)
		if err != nil {
			return nil, fmt.Errorf("fingerprint %s: %w", f, err)
		}
		out[f] = h
	}
	return out, nil
}

// StateStart returns a fresh State for base, fingerprinting every file in dirty.
func StateStart(base string, dirty []string, fp FingerprintFunc) (State, error) {
	fps, err := FingerprintAll(fp, dirty)
	if err != nil {
		return State{}, err
	}
	return State{Base: base, Dirty: fps}, nil
}

// StateGitHead returns HEAD and every dirty path (tracked changes and untracked files) of the repo at dir.
func StateGitHead(dir string) (base string, dirty []string, err error) {
	ctx := context.Background()
	if base, err = proc.RunGit(ctx, dir, "rev-parse", "HEAD"); err != nil {
		return "", nil, err
	}
	out, err := proc.RunGitRaw(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", nil, err
	}
	for _, e := range proc.StatusEntries(out) {
		dirty = append(dirty, e.Path)
		if e.From != "" {
			dirty = append(dirty, e.From)
		}
	}
	return base, dirty, nil
}

// StateLoad reads state.json; a missing file is an error wrapping os.ErrNotExist.
func StateLoad(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	return s, nil
}

// Save writes the state atomically (temp file + rename, mode 0600, parent dir created).
func (s State) Save(path string) error { return paths.WriteJSON(path, s) }

// Wave returns the record of wave n.
func (s State) Wave(n int) (WaveState, bool) {
	i := slices.IndexFunc(s.Waves, func(w WaveState) bool { return w.N == n })
	if i < 0 {
		return WaveState{}, false
	}
	return s.Waves[i], true
}

// WithWave returns a copy of s with w replacing the record of wave w.N, or appended, keeping waves sorted.
func (s State) WithWave(w WaveState) State {
	waves := slices.DeleteFunc(slices.Clone(s.Waves), func(o WaveState) bool { return o.N == w.N })
	waves = append(waves, w)
	slices.SortFunc(waves, func(a, b WaveState) int { return a.N - b.N })
	s.Waves = waves
	return s
}

// WithLeaf returns a copy of w with l replacing the leaf of the same ID, or appended.
func (w WaveState) WithLeaf(l LeafState) WaveState {
	leaves := slices.Clone(w.Leaves)
	if i := slices.IndexFunc(leaves, func(o LeafState) bool { return o.ID == l.ID }); i >= 0 {
		leaves[i] = l
	} else {
		leaves = append(leaves, l)
	}
	w.Leaves = leaves
	return w
}

// DecideAction is what to do with a leaf or a whole wave.
type DecideAction string

// Leaf actions and wave actions of DecideResult.
const (
	DecideLaunch   DecideAction = "launch"   // leaf: never recorded, or the state was discarded
	DecideKeep     DecideAction = "keep"     // leaf: ok and its owned files are unchanged
	DecideRelaunch DecideAction = "relaunch" // leaf: failed, lost, mismatched, or its owned files changed
	DecideSkip     DecideAction = "skip"     // wave: already passed
	DecideVerify   DecideAction = "verify"   // wave: every leaf kept, re-run the verifier only
	DecideRun      DecideAction = "run"      // wave: launch the leaves marked launch or relaunch
)

// DecideLeaf is the action for one leaf and the reason in a few words.
type DecideLeaf struct {
	ID     string
	Action DecideAction
	Why    string
}

// DecideResult is the resume decision of one wave; Fresh tells the caller to discard the state (StateStart).
type DecideResult struct {
	Wave   DecideAction
	Leaves []DecideLeaf
	Fresh  bool
}

// Decide applies the resume table to wave n and the planned leaf ids; fresh discards the state, so every leaf launches.
func Decide(s State, n int, ids []string, fresh bool, fp FingerprintFunc) (DecideResult, error) {
	res := DecideResult{Wave: DecideRun, Fresh: fresh}
	if fresh {
		for _, id := range ids {
			res.Leaves = append(res.Leaves, DecideLeaf{id, DecideLaunch, "fresh"})
		}
		return res, nil
	}
	w, ok := s.Wave(n)
	if ok && w.Status == WaveStatePassed {
		res.Wave = DecideSkip
		return res, nil
	}
	leaves, kept, err := decideLeaves(w, ids, fp)
	if err != nil {
		return DecideResult{}, err
	}
	res.Leaves = leaves
	if ok && kept == len(ids) && len(ids) > 0 {
		res.Wave = DecideVerify
	}
	return res, nil
}

// decideLeaves decides every planned id against wave w and counts the kept ones.
func decideLeaves(w WaveState, ids []string, fp FingerprintFunc) (out []DecideLeaf, kept int, err error) {
	for _, id := range ids {
		i := slices.IndexFunc(w.Leaves, func(l LeafState) bool { return l.ID == id })
		if i < 0 {
			out = append(out, DecideLeaf{id, DecideLaunch, "not recorded"})
			continue
		}
		d, err := decideLeaf(w.Leaves[i], fp)
		if err != nil {
			return nil, 0, err
		}
		if d.Action == DecideKeep {
			kept++
		}
		out = append(out, d)
	}
	return out, kept, nil
}

// decideLeaf is the per-leaf rows of the resume table.
func decideLeaf(l LeafState, fp FingerprintFunc) (DecideLeaf, error) {
	d := DecideLeaf{ID: l.ID, Action: DecideRelaunch}
	if l.Status != LeafStateOK {
		d.Why = "status " + l.Status
		return d, nil
	}
	if l.Verdict != "" && l.Verdict != LeafStateOK {
		d.Why = "verdict " + l.Verdict
		return d, nil
	}
	now, err := FingerprintAll(fp, l.Owns)
	if err != nil {
		return DecideLeaf{}, err
	}
	for _, f := range l.Owns {
		if now[f] != l.Fingerprints[f] {
			d.Why = "owned file changed: " + f
			return d, nil
		}
	}
	d.Action, d.Why = DecideKeep, "ok, owned files unchanged"
	return d, nil
}
