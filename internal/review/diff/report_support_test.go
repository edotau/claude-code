package diff

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestQualityCheckerUsesHeadContents(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "app.py", "def run():\n    return 1\n", "feat: add app")
	want, err := runQualityChecker(repo, "main", "feature")
	if err != nil || want.Summary.FilesAnalyzed != 1 {
		t.Fatalf("head analysis = %v, %v; want one file", want, err)
	}
	hosttest.WriteRel(t, repo, "app.py", "def run():\n"+strings.Repeat("    print('debug')\n", 100))
	got, err := runQualityChecker(repo, "main", "feature")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("worktree changed head analysis: got %v, %v; want %v", got, err, want)
	}
}

func TestChangedSourceFilesUnicode(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "config", "core.quotePath", "true")
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "café.go", "package cafe\n", "feat: cafe")
	got, err := changedSourceFiles(repo, "main", "feature")
	if err != nil || !reflect.DeepEqual(got, []string{"café.go"}) {
		t.Fatalf("source files = %q, %v; want unquoted café.go", got, err)
	}
}

func TestQualityCheckerPropagatesBlobReadFailure(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "app.py", "def run():\n    return 1\n", "feat: add app")
	failQualityBlobRead(t)
	changed, err := changedSourceFiles(repo, "main", "feature")
	if err != nil || len(changed) != 1 || changed[0] != "app.py" {
		t.Fatalf("changed source listing = %v, %v; want app.py before blob failure", changed, err)
	}
	got, err := runQualityChecker(repo, "main", "feature")
	if err == nil || got != nil {
		t.Fatalf("quality result = %v, error = %v; want no clean score after blob failure", got, err)
	}
	for _, context := range []string{"app.py", "feature", "blob-read-failed", "42"} {
		if !strings.Contains(err.Error(), context) {
			t.Errorf("error %q missing %q", err, context)
		}
	}
}

func failQualityBlobRead(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = \"show\" ]; then\n    echo blob-read-failed >&2\n    exit 42\n  fi\ndone\n" + fmt.Sprintf("exec %q \"$@\"\n", git)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
