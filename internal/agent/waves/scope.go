// Write scope of a wave's leaves: a lone leaf runs scoped; siblings run one at a time, watched for foreign writes.

package waves

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/parallel"
)

// scoped reports whether spec has no sibling in its wave, so it cannot have written another package's files.
func scoped(spec Spec, o LaunchOptions) bool {
	return !slices.ContainsFunc(o.Wave, func(s Spec) bool { return s.ID != spec.ID })
}

// launchBatch is the specs (by index) that one `agents ask --batch` runs, and whether they run scoped.
type launchBatch struct {
	idx      []int
	enforced bool
}

// launchBatches splits specs into the scoped batch run together and the unscoped ones run alone.
func launchBatches(specs []Spec, o LaunchOptions) (together, alone []launchBatch) {
	for i, spec := range specs {
		switch {
		case !scoped(spec, o):
			alone = append(alone, launchBatch{idx: []int{i}})
		case len(together) > 0:
			together[0].idx = append(together[0].idx, i)
		default:
			together = append(together, launchBatch{idx: []int{i}, enforced: true})
		}
	}
	return together, alone
}

// batchResult is one batch's envelopes (in idx order) and its error.
type batchResult struct {
	batch     launchBatch
	envelopes []Envelope
	err       error
	foreign   []CheckViolation // other packages' paths that changed while a batch run alone ran
	snapErr   error
}

// foreignKey is one path owned by a package other than the leaf being watched.
type foreignKey struct{ owner, path string }

// foreignSnapshot fingerprints every path the other packages of wave own.
func foreignSnapshot(fp FingerprintFunc, spec Spec, wave []Spec) (map[foreignKey]string, error) {
	snap := map[foreignKey]string{}
	for _, other := range wave {
		if other.ID == spec.ID {
			continue
		}
		for _, p := range other.Owns {
			h, err := fp(p)
			if err != nil {
				return nil, err
			}
			snap[foreignKey{other.ID, p}] = h
		}
	}
	return snap, nil
}

// watchAlone runs fn, a batch of spec with nothing else running, and returns the other packages' paths it changed.
func watchAlone(o LaunchOptions, spec Spec, fn func()) ([]CheckViolation, error) {
	if o.Fingerprint == nil || len(o.Wave) < 2 {
		fn()
		return nil, nil
	}
	before, err := foreignSnapshot(o.Fingerprint, spec, o.Wave)
	fn()
	if err != nil {
		return nil, fmt.Errorf("package %s: fingerprint the other packages' files: %w", spec.ID, err)
	}
	after, err := foreignSnapshot(o.Fingerprint, spec, o.Wave)
	if err != nil {
		return nil, fmt.Errorf("package %s: fingerprint the other packages' files: %w", spec.ID, err)
	}
	return foreignWrites(spec, before, after), nil
}

// foreignWrites are the paths of other packages that differ between two snapshots, written by spec alone.
func foreignWrites(spec Spec, before, after map[foreignKey]string) []CheckViolation {
	var out []CheckViolation
	for k, h := range after {
		if before[k] != h {
			out = append(out, CheckViolation{Package: spec.ID, File: k.path, Owners: []string{k.owner}})
		}
	}
	slices.SortFunc(out, func(a, b CheckViolation) int { return strings.Compare(a.File, b.File) })
	return out
}

// aloneGap is the pause before each batch run alone, so its window never shares a second with another.
var aloneGap = checkSlack

var scopeBatchRun = launchBatchRun

// launchAll runs the scoped batches together, then the others one at a time.
func launchAll(ctx context.Context, executable string, specs []Spec, o LaunchOptions) []batchResult {
	together, alone := launchBatches(specs, o)
	results := make([]batchResult, len(together)+len(alone))
	run := func(slot int, b launchBatch) error {
		res := batchResult{batch: b}
		defer func() { results[slot] = res }()
		part := make([]Spec, len(b.idx))
		for j, i := range b.idx {
			part[j] = specs[i]
		}
		do := func() { res.envelopes, res.err = scopeBatchRun(ctx, executable, part, o) }
		if b.enforced {
			do()
		} else {
			res.foreign, res.snapErr = watchAlone(o, part[0], do)
		}
		return res.err
	}
	for slot, err := range parallel.All(len(together), func(i int) error { return run(i, together[i]) }) {
		results[slot].err = err
	}
	for j, b := range alone {
		if len(together)+j > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(aloneGap):
			}
		}
		slot := len(together) + j
		results[slot].err = parallel.All(1, func(int) error { return run(slot, b) })[0]
	}
	return results
}
