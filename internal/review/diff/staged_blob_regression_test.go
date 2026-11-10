package diff

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

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

func TestRunGateHonorsAlternateIndexBlob(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n    return x\n")
	hosttest.In(t, repo, "add", "--", "app.py")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(repo, ".git", "index"))
	var stdout, stderr bytes.Buffer
	if code := RunGate([]string{"--repo", repo}, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "[WARN]") {
		t.Fatalf("simple default index: exit %d, stdout %s, stderr %s", code, &stdout, &stderr)
	}
	content := "\ndef run(x):\n" + strings.Repeat("    x += 1\n", 150) + "\n\t \n"
	hosttest.WriteRel(t, repo, "app.py", content)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "alternate-index"))
	mustRun(t, repo, "git", "add", "--", "app.py")
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n    return x\n")
	stdout.Reset()
	stderr.Reset()
	if code := RunGate([]string{"--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("alternate index gate: exit %d, stderr %s", code, &stderr)
	}
	if out := stdout.String(); !strings.Contains(out, "[WARN]") || strings.Contains(out, "detector failed") {
		t.Fatalf("missing complexity warning for alternate-index blob:\n%s", out)
	}
	got, err := stagedBlob(repo)(filepath.Join(repo, "app.py"))
	if err != nil || !bytes.Equal(got, []byte(content)) {
		t.Fatalf("alternate-index blob = %q, %v; want unchanged bytes %q", got, err, content)
	}
}

func TestRunGateReviewsLocallyRemovedStagedFile(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n"+strings.Repeat("    x += 1\n", 150))
	hosttest.In(t, repo, "add", "--", "app.py")
	if err := os.Remove(filepath.Join(repo, "app.py")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunGate([]string{"--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("gate exit = %d, stderr = %q; want advisory exit 0", code, &stderr)
	}
	out := stdout.String()
	if !strings.Contains(out, "[simplicity] complexity_checker (medium):") || !strings.Contains(out, "[WARN]") {
		t.Fatalf("missing staged complexity warning after local removal:\n%s", out)
	}
	if strings.Contains(out, "detector failed") {
		t.Fatalf("staged file must be analyzed, not reported as a detector failure:\n%s", out)
	}
}
