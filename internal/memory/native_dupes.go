package memory

import (
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/textutil"
)

// NativeDupe is one near-duplicate cluster: Keep is the survivor (the longest body — the best-evidenced
// carrier), Losers the rest. Similarity is Jaccard over the frontmatter description + body tokens.
type NativeDupe struct {
	Keep   string
	Losers []string
	Score  float64
}

// NativeDupes clusters dir's topic files whose bodies overlap at or above threshold. Report-only: naming
// a survivor is a judgement call about which one carries the better evidence, so nothing moves here.
func NativeDupes(dir string, threshold float64) []NativeDupe {
	var names []string
	sets := map[string]map[string]bool{}
	size := map[string]int{}
	for _, p := range topicPaths(dir) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
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
		cluster.Losers = without(append([]string{a}, cluster.Losers...), cluster.Keep)
		seen[cluster.Keep] = true
		out = append(out, cluster)
	}
	return out
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

// without returns names minus drop, order preserved.
func without(names []string, drop string) []string {
	var out []string
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}
