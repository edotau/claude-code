package memory

import (
	"path/filepath"
	"testing"
)

// TestSearchBanksSurfacesRoutedReposOnlyWhenNamed: another repo's blocks only enter a search's results
// when the query names that repo.
func TestSearchBanksSurfacesRoutedReposOnlyWhenNamed(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	a := gitRepo(t, t.TempDir(), "proja")
	b := gitRepo(t, t.TempDir(), "projb")
	if _, err := EnsureRepoDir(a); err != nil {
		t.Fatal(err)
	}
	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(db, "decisionLog.md"), "# D\n\n## 2026-01-01 — deploy pipeline retries three times\n\nbecause rate limits\n")

	if hits := SearchBanks(a, "deploy pipeline retries", 0); len(hits) != 0 {
		t.Errorf("unnamed repo leaked into search: %+v", hits)
	}
	hits := SearchBanks(a, "projb deploy pipeline retries", 0)
	found := false
	for _, h := range hits {
		if h.Bank == "projb" {
			found = true
		}
	}
	if !found {
		t.Errorf("naming projb did not surface its blocks: %+v", hits)
	}
}

// TestSearchAllCoversEveryRepoStore: searchAll ranks blocks across every repo store, not just one root.
func TestSearchAllCoversEveryRepoStore(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	a := gitRepo(t, t.TempDir(), "proja")
	b := gitRepo(t, t.TempDir(), "projb")
	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(da, "decisionLog.md"), "# D\n\n## 2026-01-01 — proja retries the deploy pipeline\n\nbody\n")
	write(t, filepath.Join(db, "decisionLog.md"), "# D\n\n## 2026-01-01 — projb retries the deploy pipeline\n\nbody\n")

	hits := searchAll("retries the deploy pipeline", 0)
	banks := map[string]bool{}
	for _, h := range hits {
		banks[h.Bank] = true
	}
	if !banks["proja"] || !banks["projb"] {
		t.Errorf("searchAll missed a store, banks seen = %v", banks)
	}
}
