package gemini

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// TestFitPromptDropsWholeFiles: an over-cap sweep loses whole trailing files, listed in the inventory.
func TestFitPromptDropsWholeFiles(t *testing.T) {
	var ctx Context
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		ctx.Included = append(ctx.Included, IncludedFile{Path: n, MediaType: "text/plain", Bytes: 1000, Content: strings.Repeat("x", 1000)})
	}
	prompt, got, dropped := FitPrompt("question", ctx, 3000)
	if len(prompt) > 3000 || strings.Join(dropped, ",") != "c.go" {
		t.Fatalf("len=%d dropped=%v, want fit and only c.go dropped", len(prompt), dropped)
	}
	if !strings.Contains(prompt, "c.go (prompt-cap)") || strings.Contains(prompt, `path="c.go"`) || len(got.Included) != 2 {
		t.Errorf("prompt must name c.go as skipped and inline none of it:\n%s", prompt[:300])
	}
	if p, _, d := FitPrompt("question", ctx, 1<<20); len(d) != 0 || len(p) == 0 {
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
	res, err := Run(context.Background(), Options{Cwd: dir, Task: "t", Files: []string{"a.go"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != agent.AgyDefaultModel || res.Provider != "agy" {
		t.Errorf("model/provider = %q/%q, want %q/agy", res.Model, res.Provider, agent.AgyDefaultModel)
	}
}
