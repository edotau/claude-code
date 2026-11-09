package diff

import (
	"bytes"
	"testing"
)

func TestRunScopeTextIncludesImporters(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := buildScopeRepo(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--repo", dir, "--range", "HEAD~1..HEAD", "--importers", "--format", "text"}
	if code := RunScope(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	want := "160 a/a.go\n0 b/bb.go\n0 data/blob.bin\npkg a importers: b\npkg b importers: -\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want file listing and package importers %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}
