package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCompoundMatchFindsHyphenatedAndSubstringTerms: "agent routing" finds a block that only carries
// "subagent" (agent is a substring of subagent) and "routing" exactly; a block with an exact "agent"
// outranks the substring-only match; a hyphenated query term ("agent-routing") matches both parts.
func TestCompoundMatchFindsHyphenatedAndSubstringTerms(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	repo := gitRepo(t, t.TempDir(), "routing")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "decisionLog.md"), "# Decisions\n\n"+
		"## compound candidate\nSubagent routing stays on the local classifier\n\n"+
		"## exact candidate\nAgent routing stays on the local classifier\n\n"+
		"## unrelated entry\nnothing to do with either term\n")

	hits := SearchBanks(repo, "agent routing", 0)
	var compoundScore, exactScore float64
	var compoundMatched, exactMatched int
	var compoundSeen, exactSeen bool
	for _, h := range hits {
		switch {
		case strings.Contains(h.Block, "Subagent routing"):
			compoundSeen, compoundScore, compoundMatched = true, h.Score, h.Matched
		case strings.Contains(h.Block, "Agent routing"):
			exactSeen, exactScore, exactMatched = true, h.Score, h.Matched
		}
	}
	if !compoundSeen {
		t.Fatalf("\"agent routing\" missed the compound (subagent) block: %#v", hits)
	}
	if !exactSeen {
		t.Fatalf("\"agent routing\" missed the exact block: %#v", hits)
	}
	// Matched must credit BOTH query terms on the compound block — "agent" only via the subagent
	// substring — not just the one term ("routing") that was already an exact token.
	if compoundMatched != 2 {
		t.Errorf("compound block matched %d terms, want 2 (agent via substring, routing exact): %#v", compoundMatched, hits)
	}
	if exactMatched != 2 {
		t.Errorf("exact block matched %d terms, want 2: %#v", exactMatched, hits)
	}
	if exactScore <= compoundScore {
		t.Errorf("exact match score %v did not outrank the compound match score %v", exactScore, compoundScore)
	}

	hyphen := SearchBanks(repo, "agent-routing", 0)
	var hyphenHitsCompound, hyphenHitsExact bool
	for _, h := range hyphen {
		if strings.Contains(h.Block, "Subagent routing") {
			hyphenHitsCompound = true
		}
		if strings.Contains(h.Block, "Agent routing") {
			hyphenHitsExact = true
		}
	}
	if !hyphenHitsCompound || !hyphenHitsExact {
		t.Fatalf("hyphenated query \"agent-routing\" did not match both parts: %#v", hyphen)
	}
}
