package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Every -h page lists its COMMANDS rows A-Z: the renderer sorts, so a verb added later cannot land out of order.
func TestHelpCommandsAreAlphabetical(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	vaulttest.UseFreshKey(t)
	t.Setenv("HOME", t.TempDir())
	queue := helpRootPages()
	seen := map[string]bool{}
	for len(queue) > 0 {
		path, parent := queue[0].path, queue[0].parent
		queue = queue[1:]
		key := strings.Join(path, " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		_, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return Run(append(append([]string{"claude-code"}, path...), "-h"))
		})
		text := stdout + stderr
		if text == parent {
			continue
		}
		words := commandWords(text)
		if !slices.IsSorted(words) {
			t.Errorf("%s -h: COMMANDS not alphabetical: %v", key, words)
		}
		if path[0] == "hook" || path[0] == "anthropic" { // hook -h runs modules; anthropic science forwards -h
			continue
		}
		for _, w := range words {
			queue = append(queue, helpPage{path: append(slices.Clone(path), w), parent: text})
		}
	}
}

// helpPage is a help path to render; parent is the parent page's text, since a flagless action re-prints it.
type helpPage struct {
	path   []string
	parent string
}

// helpRootPages is every visible command's completion-spec path, unparented.
func helpRootPages() []helpPage {
	var queue []helpPage
	for _, c := range commands() {
		if !c.hidden && !c.sideEffectHelp {
			for _, p := range specPaths([]string{c.name}, specTree()[c.name]) {
				queue = append(queue, helpPage{path: p})
			}
		}
	}
	return queue
}

// specPaths is path and every completion-spec sub-path under node.
func specPaths(path []string, node *helptext.Node) [][]string {
	out := [][]string{path}
	if node == nil || path[0] == "hook" || path[0] == "anthropic" {
		return out
	}
	for word, sub := range node.Sub {
		out = append(out, specPaths(append(slices.Clone(path), word), sub)...)
	}
	return out
}

// commandWords is the first word of each row under the page's COMMANDS head, in printed order.
func commandWords(text string) []string {
	var words []string
	in := false
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case line == "COMMANDS":
			in = true
		case !strings.HasPrefix(line, "  "):
			in = false
		case in && !strings.HasPrefix(line, "   "):
			if m := helpFormatRow.FindStringSubmatch(line); m != nil {
				words = append(words, strings.Fields(m[1])[0])
			}
		}
	}
	return words
}
