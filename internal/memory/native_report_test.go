package memory

import (
	"path/filepath"
	"testing"
)

// TestNativeDupesFindsOverlappingBodies: two topic files with near-identical bodies cluster, and
// the longer body survives as Keep.
func TestNativeDupesFindsOverlappingBodies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shared := "the deploy pipeline retries three times before paging oncall about the failure"
	write(t, filepath.Join(dir, "short.md"), "---\nname: short\n---\n\n"+shared+"\n")
	write(t, filepath.Join(dir, "long.md"), "---\nname: long\n---\n\n"+shared+" and then escalates to the secondary rotation\n")

	clusters, err := nativeDupes(dir, 0.6)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %+v, want exactly 1", clusters)
	}
	if clusters[0].Keep != "long.md" {
		t.Errorf("Keep = %q, want long.md (the longer body)", clusters[0].Keep)
	}
	if len(clusters[0].Losers) != 1 || clusters[0].Losers[0] != "short.md" {
		t.Errorf("Losers = %v, want [short.md]", clusters[0].Losers)
	}
}
