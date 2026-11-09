package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

// buildScopeRepo commits a two-package module, then a change: a modified file, a rename, and a binary file.
func buildScopeRepo(t *testing.T) string {
	t.Helper()
	dir := gittest.Repo(t)
	writeScopeFile(t, dir, "go.mod", "module example.com/m\n")
	writeScopeFile(t, dir, "a/a.go", "package a\n\nfunc A() int { return 1 }\n")
	writeScopeFile(t, dir, "b/b.go", "package b\n\nimport \"example.com/m/a\"\n\nvar _ = a.A\n")
	gittest.In(t, dir, "add", "-A")
	gittest.In(t, dir, "commit", "-q", "-m", "base")

	writeScopeFile(t, dir, "a/a.go", "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n")
	gittest.In(t, dir, "mv", "b/b.go", "b/bb.go")
	writeScopeFile(t, dir, "data/blob.bin", "\x00\x01\x02")
	gittest.In(t, dir, "add", "-A")
	gittest.In(t, dir, "commit", "-q", "-m", "change")
	return dir
}

func writeScopeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunScopeEndToEnd(t *testing.T) {
	dir := buildScopeRepo(t)
	var stdout, stderr bytes.Buffer
	code := RunScope([]string{"--repo", dir, "--range", "HEAD~1..HEAD", "--importers", "--format", "json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var got scopeResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	want := scopeResult{
		Files: []scopeFile{
			{Path: "a/a.go", Bytes: 160}, // two added lines
			{Path: "b/bb.go", Bytes: 0},  // pure rename: the new path, no lines
			{Path: "data/blob.bin", Bytes: 0},
		},
		Packages: []scopePackage{
			{Dir: "a", Files: []string{"a/a.go"}, Importers: []string{"b"}},
			{Dir: "b", Files: []string{"b/bb.go"}, Importers: []string{}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestScopeImportersWithSpaceInRepoPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my repo")
	writeScopeFile(t, dir, "go.mod", "module example.com/m\n")
	writeScopeFile(t, dir, "a/a.go", "package a\n")
	writeScopeFile(t, dir, "b/b.go", "package b\nimport _ \"example.com/m/a\"\n")
	got, err := goImporters(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"a": {"b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("importers = %v, want %v", got, want)
	}
}

func TestScopeExplicitExcludedRoot(t *testing.T) {
	for _, excluded := range []string{"vendor", "node_modules", ".venv", "testdata"} {
		t.Run(excluded, func(t *testing.T) {
			dir := t.TempDir()
			writeScopeFile(t, dir, excluded+"/lib.go", "package lib\n")
			writeScopeFile(t, dir, excluded+"/nested/lib.go", "package lib\n")
			writeScopeFile(t, dir, "keep.go", "package keep\n")
			for _, root := range []string{excluded, excluded + "/lib.go", excluded + "/nested", "."} {
				t.Run(root, func(t *testing.T) {
					got, err := scopeFiles(dir, "", []string{root, "keep.go"})
					if err != nil {
						t.Fatal(err)
					}
					want := []scopeFile{{Path: "keep.go", Bytes: int64(len("package keep\n"))}}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("source files under %q = %v, want %v", root, got, want)
					}
				})
			}
		})
	}
}

func TestScopeSourceSymlinks(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("x", 1000)
	writeScopeFile(t, dir, "real.go", body)
	if err := os.Symlink("real.go", filepath.Join(dir, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.txt", filepath.Join(dir, "dangling.go")); err != nil {
		t.Fatal(err)
	}
	got, err := scopeFiles(dir, "", []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	want := []scopeFile{
		{Path: "alias.go", Bytes: int64(len(body))},
		{Path: "real.go", Bytes: int64(len(body))},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symlink files = %v, want target size and no dangling link: %v", got, want)
	}
}

func TestRunScopeValidationUsesStderr(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid format", []string{"--format", "xml", "--range", "HEAD~1..HEAD"}, "xml"},
		{"missing range and paths", nil, "scope needs --range or --paths"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := RunScope(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRunScopeEdges(t *testing.T) {
	dir := buildScopeRepo(t)

	t.Run("empty range lists no files", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunScope([]string{"--repo", dir, "--range", "HEAD..HEAD", "--format", "json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"files": []`) {
			t.Errorf("want empty files array, got %s", stdout.String())
		}
	})

	t.Run("paths lists source files only, sized on disk", func(t *testing.T) {
		info, err := os.Stat(filepath.Join(dir, "a", "a.go"))
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := RunScope([]string{"--repo", dir, "--paths", "a,data"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
		if want := fmt.Sprintf("%d a/a.go\n", info.Size()); stdout.String() != want {
			t.Errorf("stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("bad range exits 1 with Error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := RunScope([]string{"--repo", dir, "--range", "nosuchref..HEAD"}, &stdout, &stderr); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.HasPrefix(stderr.String(), "Error: ") {
			t.Errorf("stderr = %q, want an Error: line", stderr.String())
		}
	})
}
