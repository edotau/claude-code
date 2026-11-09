package memstore

import (
	"os"
	"path/filepath"
	"testing"
)

func seedRouteStore(t *testing.T, key, origin string) string {
	t.Helper()
	dir := filepath.Join(StoreDir(), key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: key, Origin: origin}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRepoByNamePrefersExactKey(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	seedRouteStore(t, "alpha", "/src/bravo")
	want := seedRouteStore(t, "bravo", "/other/checkout")

	got, ok := RepoByName("bravo")
	if !ok || got.Key != "bravo" || got.Dir != want || got.Origin.Origin != "/other/checkout" {
		t.Fatalf("RepoByName(bravo) = %+v, %v; want exact key bravo at %s, not alpha's origin basename", got, ok, want)
	}
}

func TestRoutedReposMatchesDottedName(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	want := seedRouteStore(t, ".project", "/src/.project")

	got := RoutedRepos("check .project")
	if len(got) != 1 {
		t.Fatalf("RoutedRepos(check .project) = %+v; want exactly the dotted store", got)
	}
	if got[0].Key != ".project" || got[0].Dir != want {
		t.Fatalf("RoutedRepos(check .project) = %+v; want .project at %s", got, want)
	}
}
