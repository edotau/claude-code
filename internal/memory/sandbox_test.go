package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestSandboxEmptyAndUnknownRepo(t *testing.T) {
	tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "checkout")
	b := BankFor(repo)
	if _, err := Init(repo); err != nil {
		t.Fatal(err)
	}
	write(t, b.Path("topic.md"), "do not copy")
	if _, err := Sandbox(SandboxOptions{Repo: "unknown-sandbox-store"}); err == nil {
		t.Fatal("unknown repo accepted")
	}
	res, err := Sandbox(SandboxOptions{Repo: repo, Empty: true})
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
		wantFile(t, filepath.Join(mem, b.Prefix()+name), body)
	}
	wantFile(t, filepath.Join(mem, b.Prefix()+"topic.md"), "")
}

func TestSandboxRefusesUnstampedStore(t *testing.T) {
	tempConfig(t)
	write(t, filepath.Join(StoreDir(), "alpha", "progress.md"), "x\n")
	if _, err := Sandbox(SandboxOptions{Empty: true}); !errors.Is(err, errStoreNotFlat) {
		t.Fatalf("Sandbox on a per-repo store = %v, want errStoreNotFlat", err)
	}
}

func TestSandboxFlatCopiesOwnFilesAndWritesNoOrigins(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "checkout")
	b := BankFor(repo)
	for _, f := range []string{b.Path("progress.md"), HomeBank().Path("progress.md"), Bank{Key: "other"}.Path("progress.md"),
		b.Sub("reports", "r.md"), Bank{Key: "other"}.Sub("reports", "r.md"),
		b.Sub("reports", "bundle/sub/review.md"), b.Sub("waves", "plan/state.json"), b.Sub("archive", "bk/day/MEMORY.md"),
		Bank{Key: "other"}.Sub("reports", "bundle/sub/review.md")} {
		write(t, f, "x")
	}
	res, err := Sandbox(SandboxOptions{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(res.Dir) })
	mem := filepath.Join(res.Dir, "memory")
	for _, rel := range []string{b.Prefix() + "progress.md", "progress.md", "reports/" + b.Prefix() + "r.md", layoutFile,
		"reports/" + b.Prefix() + "bundle/sub/review.md", "waves/" + b.Prefix() + "plan/state.json", "archive/" + b.Prefix() + "bk/day/MEMORY.md"} {
		if !statOK(filepath.Join(mem, rel)) {
			t.Errorf("sandbox lacks %s", rel)
		}
	}
	for _, rel := range []string{"other--progress.md", "reports/other--r.md", "reports/other--bundle/sub/review.md"} {
		if statOK(filepath.Join(mem, rel)) {
			t.Errorf("sandbox copied another bank's %s", rel)
		}
	}
	if statOK(filepath.Join(res.Dir, "state", "vault.db")) {
		t.Error("a flat sandbox must write no origin rows")
	}
	_ = cfg
}
