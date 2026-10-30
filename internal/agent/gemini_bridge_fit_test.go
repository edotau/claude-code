package agent

import (
	"context"
	"github.com/edotau/claude-code/internal/agent/vendors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFitPromptDropsWholeFiles: an over-cap sweep loses whole trailing files, listed in the inventory.
func TestFitPromptDropsWholeFiles(t *testing.T) {
	var ctx Context
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		ctx.Included = append(ctx.Included, IncludedFile{Path: n, MediaType: "text/plain", Bytes: 1000, Content: strings.Repeat("x", 1000)})
	}
	prompt, got, dropped, _ := FitPrompt(constTask("question"), ctx, 3000)
	if len(prompt) > 3000 || strings.Join(dropped, ",") != "c.go" {
		t.Fatalf("len=%d dropped=%v, want fit and only c.go dropped", len(prompt), dropped)
	}
	if !strings.Contains(prompt, "c.go (prompt-cap)") || strings.Contains(prompt, `path="c.go"`) || len(got.Included) != 2 {
		t.Errorf("prompt must name c.go as skipped and inline none of it:\n%s", prompt[:300])
	}
	if p, _, d, _ := FitPrompt(constTask("question"), ctx, 1<<20); len(d) != 0 || len(p) == 0 {
		t.Errorf("under the cap nothing may drop, got %v", d)
	}
}

// TestBridgeRunAgyModelFilled: the agy bridge envelope carries the resolved model, not an empty field.
func TestBridgeRunAgyModelFilled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	fakeAgyScript(t, filepath.Join(t.TempDir(), "ran"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: dir, Task: "t", Files: []string{"a.go"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != vendors.AgyDefaultModel || res.Provider != "agy" {
		t.Errorf("model/provider = %q/%q, want %q/agy", res.Model, res.Provider, vendors.AgyDefaultModel)
	}
}

func constTask(s string) func(Context) string { return func(Context) string { return s } }

// TestFitPromptOverCapWithoutFiles: a task that alone exceeds the cap errors instead of dispatching.
func TestFitPromptOverCapWithoutFiles(t *testing.T) {
	ctx := Context{Included: []IncludedFile{{Path: "a.go", MediaType: "text/plain", Bytes: 5, Content: "hello"}}}
	if _, _, _, err := FitPrompt(constTask(strings.Repeat("q", 5000)), ctx, 3000); err == nil {
		t.Fatal("want an error when even zero files exceed the cap")
	}
}

// TestFitPromptIndexTracksTrim: the index built per trim marks dropped files as not inlined.
func TestFitPromptIndexTracksTrim(t *testing.T) {
	var ctx Context
	for _, n := range []string{"a.go", "b.go"} {
		ctx.Included = append(ctx.Included, IncludedFile{Path: n, MediaType: "text/plain", Bytes: 1000, Content: strings.Repeat("x", 1000)})
	}
	task := func(c Context) string { return "q" + IndexPrompt(c) }
	prompt, _, dropped, err := FitPrompt(task, ctx, len(BuildPrompt(task(ctx), ctx))-500)
	if err != nil || len(dropped) != 1 {
		t.Fatalf("dropped=%v err=%v, want one dropped file", dropped, err)
	}
	if !strings.Contains(prompt, `"body": "prompt-cap"`) {
		t.Errorf("index must mark the dropped file prompt-cap:\n%s", prompt)
	}
}
