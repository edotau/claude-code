package pack

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestChangedFilesErrorsOutsideRepo: called outside a git work tree, the error mentions git rather
// than masking the failure as an empty diff.
func TestChangedFilesErrorsOutsideRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, err := changedFiles(t.Context(), dir)
	if err == nil {
		t.Fatal("expected an error outside a git work tree")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("err = %v, want it to mention git", err)
	}
}

// TestBuildIndexOnlyInlinesNoBodies: IndexOnly packs the index of every path and no <file> body.
func TestBuildIndexOnlyInlinesNoBodies(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, "b.go"), "package b")
	got, err := Build(t.Context(), Spec{Dirs: []string{"."}, Index: true, IndexOnly: true}, "t", dir, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Prompt, "<file path=") || !strings.Contains(got.Prompt, "a.go") || !strings.Contains(got.Prompt, "b.go") {
		t.Errorf("want both paths in the index and no file body:\n%s", got.Prompt)
	}
}

// TestBuildDiffInlinesOnlyChanged: Diff packs only files changed vs HEAD and refuses once nothing changed.
func TestBuildDiffInlinesOnlyChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	write(t, filepath.Join(repo, "base.txt"), "base content")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	git("add", ".")
	git("commit", "-m", "base")
	write(t, filepath.Join(repo, "new.txt"), "untracked content")

	got, err := Build(t.Context(), Spec{Diff: true, MaxFiles: -1}, "t", repo, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Prompt; !strings.Contains(p, `<file path="new.txt"`) || strings.Contains(p, `<file path="base.txt"`) {
		t.Errorf("want only new.txt inlined:\n%s", p)
	}
	git("add", ".")
	git("commit", "-m", "new")
	if _, err = Build(t.Context(), Spec{Diff: true}, "t", repo, testBudget); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("err = %v, want 'nothing changed'", err)
	}
}

// TestBuildLargeFileAccepted: a 200 KiB inlined file is packed whole, not capped.
func TestBuildLargeFileAccepted(t *testing.T) {
	const bigFile = 200 << 10
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", bigFile))
	got, err := Build(t.Context(), Spec{Dirs: []string{"."}, MaxFiles: -1, MaxFileBytes: 2 * bigFile}, "t", dir, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Prompt; !strings.Contains(p, `<file path="big.txt"`) || len(p) < bigFile {
		t.Errorf("big.txt not inlined whole (prompt %d bytes)", len(p))
	}
}

// TestBuildPrompt: template contains inventory, file blocks, task, and constraints.
func TestBuildPrompt(t *testing.T) {
	ctx := Context{
		Included: []IncludedFile{
			{Path: "a.go", MediaType: "text/plain", Bytes: 8, Truncated: false, Content: "package a"},
			{Path: "big.txt", MediaType: "text/plain", Bytes: 100, Truncated: true, Content: "partial"},
		},
		Skipped: []SkippedFile{{Path: "x.png", Reason: "unsupported-extension"}},
	}
	got := prompt("do a thing", ctx)
	for _, want := range []string{
		"<context_inventory>",
		"- a.go | text/plain | 8 bytes | truncated=false",
		"Skipped files:",
		"- x.png (unsupported-extension)",
		`<file path="a.go" media_type="text/plain" truncated="false">package a</file>`,
		"- big.txt | text/plain | 100 bytes | truncated=true",
		`<file path="big.txt" media_type="text/plain" truncated="true">partial</file>`,
		"<task>\ndo a thing\n</task>",
		"Do not invent files",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// TestBuildPromptEmpty: no files → "Included files: none" + no-payload sentinel.
func TestBuildPromptEmpty(t *testing.T) {
	got := prompt("task", Context{})
	if !strings.Contains(got, "Included files: none") {
		t.Error("expected 'Included files: none'")
	}
	if !strings.Contains(got, "No inline file payloads were collected.") {
		t.Error("expected no-payload sentinel")
	}
}

// TestBuildIndexNests: a flat path list costs one token per path segment repeated on every line. A
// nested tree collapses shared prefixes, which is what lets a whole repo fit where inlining files
// cannot — the gemini's "max-files-exceeded" failure was exactly this budget running out.
func TestBuildIndexNests(t *testing.T) {
	idx := index(Context{
		Included: []IncludedFile{{Path: "internal/router/dispatch.go", Bytes: 100}},
		Skipped:  []SkippedFile{{Path: "internal/cli/run.go", Reason: "max-files-exceeded"}},
	})
	var tree map[string]any
	if err := json.Unmarshal([]byte(idx), &tree); err != nil {
		t.Fatalf("index is not valid JSON: %v\n%s", err, idx)
	}
	internal, ok := tree["internal"].(map[string]any)
	if !ok {
		t.Fatalf("index is not nested by directory: %s", idx)
	}
	if _, ok := internal["router"].(map[string]any); !ok {
		t.Errorf("second level missing: %s", idx)
	}
}

// TestIndexCoversSkippedFiles: a file that was too big to inline must STILL appear in the index.
// Omitting it is what made the gemini answer "contents are not available" — it could not even see
// that the file existed, so it could not ask for it by name.
func TestIndexCoversSkippedFiles(t *testing.T) {
	idx := index(Context{Skipped: []SkippedFile{{Path: "internal/router/spillpool.go", Reason: "max-files-exceeded"}}})
	if !strings.Contains(idx, "spillpool.go") {
		t.Errorf("skipped file absent from the index — it becomes invisible, not merely unread:\n%s", idx)
	}
}

// TestIndexCarriesMetadata: the index is a routing aid, so each leaf needs enough to decide whether
// the file is worth pulling — size, and whether its contents were actually included.
func TestIndexCarriesMetadata(t *testing.T) {
	idx := index(Context{Included: []IncludedFile{{Path: "a/b.go", Bytes: 4096}}})
	for _, want := range []string{"4096", "b.go"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index leaf missing %q:\n%s", want, idx)
		}
	}
}

// TestFitPromptDropsWholeFiles: an over-cap sweep loses whole trailing files, listed in the inventory.
func TestFitPromptDropsWholeFiles(t *testing.T) {
	var ctx Context
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		ctx.Included = append(ctx.Included, IncludedFile{Path: n, MediaType: "text/plain", Bytes: 1000, Content: strings.Repeat("x", 1000)})
	}
	prompt, got, dropped, _ := fit(constTask("question"), ctx, 3000)
	if len(prompt) > 3000 || strings.Join(dropped, ",") != "c.go" {
		t.Fatalf("len=%d dropped=%v, want fit and only c.go dropped", len(prompt), dropped)
	}
	if !strings.Contains(prompt, "c.go (prompt-cap)") || strings.Contains(prompt, `path="c.go"`) || len(got.Included) != 2 {
		t.Errorf("prompt must name c.go as skipped and inline none of it:\n%s", prompt[:300])
	}
	if p, _, d, _ := fit(constTask("question"), ctx, 1<<20); len(d) != 0 || len(p) == 0 {
		t.Errorf("under the cap nothing may drop, got %v", d)
	}
}

func constTask(s string) func(Context) string { return func(Context) string { return s } }

// TestFitPromptOverCapWithoutFiles: a task that alone exceeds the cap errors instead of dispatching.
func TestFitPromptOverCapWithoutFiles(t *testing.T) {
	ctx := Context{Included: []IncludedFile{{Path: "a.go", MediaType: "text/plain", Bytes: 5, Content: "hello"}}}
	if _, _, _, err := fit(constTask(strings.Repeat("q", 5000)), ctx, 3000); err == nil {
		t.Fatal("want an error when even zero files exceed the cap")
	}
}

// TestFitPromptIndexTracksTrim: the index built per trim marks dropped files as not inlined.
func TestFitPromptIndexTracksTrim(t *testing.T) {
	var ctx Context
	for _, n := range []string{"a.go", "b.go"} {
		ctx.Included = append(ctx.Included, IncludedFile{Path: n, MediaType: "text/plain", Bytes: 1000, Content: strings.Repeat("x", 1000)})
	}
	task := func(c Context) string { return "q" + indexPrompt(c) }
	prompt, _, dropped, err := fit(task, ctx, len(prompt(task(ctx), ctx))-500)
	if err != nil || len(dropped) != 1 {
		t.Fatalf("dropped=%v err=%v, want one dropped file", dropped, err)
	}
	if !strings.Contains(prompt, `"body": "prompt-cap"`) {
		t.Errorf("index must mark the dropped file prompt-cap:\n%s", prompt)
	}
}

func TestChangedFilesCancellationMentionsGit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := changedFiles(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "git") {
		t.Fatalf("error = %v, want wrapped git cancellation", err)
	}
}
