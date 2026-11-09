package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/textutil"
)

// NativeDupe is one near-duplicate cluster: Keep is the survivor (the longest body — the best-evidenced
// carrier), Losers the rest. Similarity is Jaccard over the frontmatter description + body tokens.
type NativeDupe struct {
	Keep   string
	Losers []string
	Score  float64
}

// NativeDupes is nativeDupes with its error dropped; an unreadable topic reads as no overlap, so use nativeDupes.
func NativeDupes(dir string, threshold float64) []NativeDupe {
	out, _ := nativeDupes(dir, threshold)
	return out
}

// nativeDupes clusters dir's topic files whose bodies overlap at or above threshold; an unreadable topic fails the
// scan, since a file never read cannot be ruled out as a duplicate. Report-only: nothing moves here.
func nativeDupes(dir string, threshold float64) ([]NativeDupe, error) {
	topics, err := topicFiles(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	sets := map[string]map[string]bool{}
	size := map[string]int{}
	for _, p := range topics {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		name := filepath.Base(p)
		names = append(names, name)
		sets[name] = textutil.TokenSet(string(b))
		size[name] = len(b)
	}
	seen := map[string]bool{}
	var out []NativeDupe
	for i, a := range names {
		if seen[a] {
			continue
		}
		cluster := dupeCluster(a, names[i+1:], sets, seen, threshold)
		if len(cluster.Losers) == 0 {
			continue
		}
		// The longest body wins: a memory carrying more evidence is the better carrier to merge into.
		for _, c := range append([]string{cluster.Keep}, cluster.Losers...) {
			if size[c] > size[cluster.Keep] {
				cluster.Keep = c
			}
		}
		cluster.Losers = slices.DeleteFunc(append([]string{a}, cluster.Losers...), func(n string) bool { return n == cluster.Keep })
		seen[cluster.Keep] = true
		out = append(out, cluster)
	}
	return out, nil
}

// dupeCluster gathers the unseen names whose token sets overlap a's at or above threshold, marking them seen.
func dupeCluster(a string, rest []string, sets map[string]map[string]bool, seen map[string]bool, threshold float64) NativeDupe {
	cluster := NativeDupe{Keep: a}
	for _, b := range rest {
		if seen[b] {
			continue
		}
		if s := textutil.Jaccard(sets[a], sets[b]); s >= threshold {
			cluster.Losers = append(cluster.Losers, b)
			seen[b] = true
			cluster.Score = max(cluster.Score, s)
		}
	}
	return cluster
}
