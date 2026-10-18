package crossgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from cwd to the module root (go.mod) so the guards see the real roster.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test dir")
		}
		dir = parent
	}
}

// Every agents/<name>.md must declare `name:` equal to its file stem — the invariant memory routing depends on.
func TestRosterNamesMatchDirs(t *testing.T) {
	agents, err := DiscoverAgents(filepath.Join(repoRoot(t), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) == 0 {
		t.Skip("empty roster — not the harness checkout")
	}
	for _, a := range agents {
		if a.frontmatter["name"] != a.dirName {
			t.Errorf("agents/%s.md: name = %v, want %q", a.dirName, a.frontmatter["name"], a.dirName)
		}
	}
}

// An unknown tier flows verbatim into every generated surface, so a typo or a provider name is never caught.
func TestRosterModelTiersAreValid(t *testing.T) {
	valid := map[string]bool{"opus": true, "sonnet": true, "haiku": true, "fable": true, "inherit": true}
	agents, err := DiscoverAgents(filepath.Join(repoRoot(t), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, a := range agents {
		tier, ok := a.frontmatter["model"]
		if !ok {
			continue // no model: key means inherit
		}
		if s, _ := tier.(string); !valid[s] {
			t.Errorf("agents/%s.md: model %v is not a tier (opus|sonnet|haiku|fable|inherit)", a.dirName, tier)
		}
		checked++
	}
	if len(agents) == 0 {
		t.Skip("empty roster — not the harness checkout")
	}
	if checked == 0 {
		t.Fatal("no agent declared a model tier — the guard would pass vacuously")
	}
}

// TestNoStaleReferencesToRemovedAgents guards against a dangling pointer into an agent file that no
// longer exists — the roster drops agents (sonnet, documentation, reflection) outright, with no shim.
func TestNoStaleReferencesToRemovedAgents(t *testing.T) {
	root := repoRoot(t)
	agentsDir := filepath.Join(root, "agents")
	if _, err := os.Stat(agentsDir); err != nil {
		t.Skip("no agents/ dir — not the harness checkout")
	}
	removed := []string{"agents/sonnet.md", "agents/documentation.md", "agents/reflection.md"}
	for _, stale := range removed {
		if _, err := os.Stat(filepath.Join(root, stale)); err == nil {
			t.Fatalf("%s still exists — update the removed list, not this guard", stale)
		}
	}
	for _, dir := range []string{"rules", "commands", "workflows"} {
		scanDir := filepath.Join(root, dir)
		err := filepath.Walk(scanDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, stale := range removed {
				if strings.Contains(string(data), stale) {
					t.Errorf("%s references removed %s", path, stale)
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}
