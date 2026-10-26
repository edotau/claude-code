package memory

import (
	"path/filepath"
	"testing"
)

// TestRoutedReposWholeWordMatchAndShortKeysIgnored: a routed name must appear in the query as a whole
// word; a bare substring of it, or a key under 4 chars, never matches.
func TestRoutedReposWholeWordMatchAndShortKeysIgnored(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "edotau.github.io")
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}
	short := gitRepo(t, t.TempDir(), "abc")
	if _, err := EnsureRepoDir(short); err != nil {
		t.Fatal(err)
	}

	got := RoutedRepos("check edotau.github.io for the deploy")
	if len(got) != 1 || filepath.Base(got[0].Origin.Origin) != "edotau.github.io" {
		t.Fatalf("got = %+v, want exactly the edotau.github.io entry", got)
	}

	if got := RoutedRepos("just github alone"); len(got) != 0 {
		t.Errorf("a bare substring of a routed name matched: %+v", got)
	}
	if got := RoutedRepos("abc is here"); len(got) != 0 {
		t.Errorf("a key under 4 chars matched: %+v", got)
	}
	if got := RoutedRepos("nothing names a repo here"); len(got) != 0 {
		t.Errorf("an unrelated query matched: %+v", got)
	}
}

// TestRoutedFilesExcludesOwnStore checks that naming both stores only loads the other's blocks.
func TestRoutedFilesExcludesOwnStore(t *testing.T) {
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
	write(t, filepath.Join(da, "decisionLog.md"), "# D\n\n## 2026-01-01 — proja note\n\nbody\n")
	write(t, filepath.Join(db, "decisionLog.md"), "# D\n\n## 2026-01-01 — projb note\n\nbody\n")

	files := routedFiles(a, "proja projb")
	var blocks []SearchHit
	for i, seg := range loadSegments(files, false) {
		if seg == nil {
			continue
		}
		for j := 0; j < seg.docs(); j++ {
			block, _ := seg.doc(j)
			blocks = append(blocks, SearchHit{Bank: files[i].bank, File: filepath.Base(files[i].path), Block: block})
		}
	}
	if len(blocks) == 0 {
		t.Fatal("routedFiles loaded no blocks for projb")
	}
	for _, blk := range blocks {
		if blk.Bank == "proja" {
			t.Errorf("own store leaked into routedFiles: %+v", blk)
		}
	}
}

// TestRepoByNameMatchesKeyOrOriginBasename: RepoByName folds case and matches either the store key or
// the origin's basename.
func TestRepoByNameMatchesKeyOrOriginBasename(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "proja")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := RepoByName("proja"); !ok || e.Dir != dir {
		t.Fatalf("RepoByName(key) = %+v, %v", e, ok)
	}
	if e, ok := RepoByName("PROJA"); !ok || e.Dir != dir {
		t.Fatalf("RepoByName must fold case, got %+v, %v", e, ok)
	}
	if _, ok := RepoByName("nope"); ok {
		t.Error("RepoByName matched a name no store has")
	}
}

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

// TestSearchAllCoversEveryRepoStore: SearchAll ranks blocks across every repo store, not just one root.
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

	hits := SearchAll("retries the deploy pipeline", 0)
	banks := map[string]bool{}
	for _, h := range hits {
		banks[h.Bank] = true
	}
	if !banks["proja"] || !banks["projb"] {
		t.Errorf("SearchAll missed a store, banks seen = %v", banks)
	}
}
