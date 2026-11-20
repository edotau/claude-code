package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// A messy registry (blank, duplicate, trailing slash, CRLF, relative, gone) keeps only the real absolute dirs.
func TestUpdateSandboxRegistryTidiesMessyFile(t *testing.T) {
	tempConfig(t)
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	write(t, SandboxRegistry(), "\n"+a+"\n\n"+a+"\n"+b+"/\n.\n"+c+"\r\n"+gone+"\n")
	live, err := UpdateSandboxRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range live {
		got[filepath.Clean(d)] = true
	}
	if want := map[string]bool{a: true, b: true, c: true}; len(live) != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("live = %q, want one entry each of %q", live, []string{a, b, c})
	}
	data, err := os.ReadFile(SandboxRegistry())
	if err != nil || string(data) != strings.Join(live, "\n")+"\n" {
		t.Errorf("registry rewritten as %q (%v), want the live entries one per line, no blanks", data, err)
	}
}

func TestSandboxEmpty(t *testing.T) {
	tempConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, Path("topic.md"), "do not copy")
	res, err := Sandbox(SandboxOptions{Empty: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(res.Dir); err != nil {
			t.Error(err)
		}
	})
	mem := filepath.Join(res.Dir, "memory")
	for name, body := range templates() {
		wantFile(t, filepath.Join(mem, name), body)
	}
	wantFile(t, filepath.Join(mem, "topic.md"), "")
}

func TestSandboxRefusesUnstampedStore(t *testing.T) {
	tempConfig(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "alpha", "progress.md"), "x\n")
	if _, err := Sandbox(SandboxOptions{Empty: true}); !errors.Is(err, errStoreNotFlat) {
		t.Fatalf("Sandbox on a per-repo store = %v, want errStoreNotFlat", err)
	}
}

func TestSandboxCopiesTheOneBankAndWritesNoOrigins(t *testing.T) {
	tempConfig(t)
	rels := []string{"progress.md", "topic.md", "reports--r.md", "reports--bundle--sub--review.md", "waves--plan--state.json"}
	for _, rel := range rels {
		write(t, filepath.Join(paths.MemoryStoreDir(), rel), "x")
	}
	res, err := Sandbox(SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(res.Dir) })
	mem := filepath.Join(res.Dir, "memory")
	for _, rel := range append(rels, layoutFile) {
		if !statOK(filepath.Join(mem, rel)) {
			t.Errorf("sandbox lacks %s", rel)
		}
	}
	if len(res.Stores) != 1 || res.Stores[0].Key != stateKey() {
		t.Errorf("sandbox stores = %+v, want the one bank", res.Stores)
	}
}

// D6: a sandbox of an unstamped flat store must not write the layout stamp into the source store.
func TestSandboxLeavesSourceUnstamped(t *testing.T) {
	tempConfig(t)
	write(t, Path("progress.md"), "x\n")
	res, err := Sandbox(SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(res.Dir) })
	if statOK(filepath.Join(paths.MemoryStoreDir(), layoutFile)) {
		t.Error("Sandbox stamped the source store")
	}
	if !statOK(Path("progress.md")) { // a copy, not a move
		t.Error("Sandbox removed progress.md from the source store")
	}
}

// D8: a file listed but deleted before its copy is skipped and dropped from the result.
func TestSandboxCopySkipsVanishedFile(t *testing.T) {
	tempConfig(t)
	write(t, Path("progress.md"), "x\n")
	dst := t.TempDir()
	got, err := placeStoreRels(dst, []string{"gone.md", "progress.md"}, false)
	if err != nil || !reflect.DeepEqual(got, []string{"progress.md"}) {
		t.Fatalf("placeStoreRels = %q, %v; want [progress.md], nil", got, err)
	}
	wantFile(t, filepath.Join(dst, "progress.md"), "x\n")
	if statOK(filepath.Join(dst, "gone.md")) || statOK(Path("gone.md")) {
		t.Error("gone.md exists in the sandbox or the source; it was deleted before its copy")
	}
}
