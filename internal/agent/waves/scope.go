// Write scope of a wave's leaves: each leaf launches denied Edit and Write on every other package's files.

package waves

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// scopeEnforcers hold a path-scoped Edit/Write deny; codex's OS sandbox reads no harness rule.
var scopeEnforcers = map[string]bool{"claude": true, "copilot": true, "gemini": true, "opencode": true}

// ScopeEnforced reports whether agent refuses a write outside its package's files when launched with scopeFlags.
func ScopeEnforced(agent string) bool { return scopeEnforcers[agent] }

// scopeRules denies Edit/Write on the other packages' files; clean is false when one overlaps spec's own (left out).
func scopeRules(spec Spec, wave []Spec) (rules []string, clean bool) {
	own, clean := checkScope(spec.Owns), true
	for _, other := range wave {
		if other.ID == spec.ID {
			continue
		}
		for _, p := range checkScope(other.Owns) {
			if slices.ContainsFunc(own, func(o string) bool { return scopeOverlap(o, p) }) {
				clean = false
				continue
			}
			for _, arg := range []string{p, p + "/**"} {
				for _, tool := range []string{"Edit", "Write"} {
					if rule := tool + "(" + arg + ")"; !slices.Contains(rules, rule) {
						rules = append(rules, rule)
					}
				}
			}
		}
	}
	return rules, clean
}

func scopeOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// scopeFlags is the ask flags confining spec to its own files, and whether that is enforced (else detection only).
func scopeFlags(spec Spec, o LaunchOptions) (flags []string, enforced bool) {
	if !slices.ContainsFunc(o.Wave, func(s Spec) bool { return s.ID != spec.ID }) {
		return nil, true
	}
	if !ScopeEnforced(spec.Agent) {
		return nil, false
	}
	rules, clean := scopeRules(spec, o.Wave)
	for _, rule := range rules {
		flags = append(flags, "--deny", rule)
	}
	if !slices.Contains(o.Route, "--mode") { // a denied launch with no mode asks for the rest: opencode's judge would
		flags = append(flags, "--mode", "auto")
	}
	return flags, clean
}

// launchBatch is the specs (by index) that one `agents ask --batch` runs, with their shared scope flags.
type launchBatch struct {
	idx      []int
	flags    []string
	enforced bool
}

// launchBatches splits specs into batches run together (scoped) and run alone (unscoped, so windows name the writer).
func launchBatches(specs []Spec, o LaunchOptions) (together, alone []launchBatch) {
	byFlags := map[string]int{}
	for i, spec := range specs {
		flags, enforced := scopeFlags(spec, o)
		key := strings.Join(flags, "\x00")
		switch at, seen := byFlags[key]; {
		case !enforced:
			alone = append(alone, launchBatch{idx: []int{i}})
		case seen:
			together[at].idx = append(together[at].idx, i)
		default:
			byFlags[key] = len(together)
			together = append(together, launchBatch{idx: []int{i}, flags: flags, enforced: true})
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

// launchAll runs the scoped batches together, then the others one at a time.
func launchAll(ctx context.Context, executable string, specs []Spec, o LaunchOptions) []batchResult {
	together, alone := launchBatches(specs, o)
	results := make([]batchResult, len(together)+len(alone))
	run := func(slot int, b launchBatch) {
		part := make([]Spec, len(b.idx))
		for j, i := range b.idx {
			part[j] = specs[i]
		}
		res := batchResult{batch: b}
		do := func() { res.envelopes, res.err = launchBatchRun(ctx, executable, part, b.flags, o) }
		if b.enforced {
			do()
		} else {
			res.foreign, res.snapErr = watchAlone(o, part[0], do)
		}
		results[slot] = res
	}
	var wg sync.WaitGroup
	for slot, b := range together {
		wg.Go(func() { run(slot, b) })
	}
	wg.Wait()
	for j, b := range alone {
		if len(together)+j > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(aloneGap):
			}
		}
		run(len(together)+j, b)
	}
	return results
}
