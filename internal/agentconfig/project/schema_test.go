package project

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
)

// scriptRefPattern finds a scripts/<name>.sh reference anywhere in a command file's text.
var scriptRefPattern = regexp.MustCompile(`scripts/[A-Za-z0-9_.-]+\.sh`)

// argPlaceholderPattern matches $ARGUMENTS or a positional $1-$9, neither of which Claude Code
// substitutes inside a !`...` directive.
var argPlaceholderPattern = regexp.MustCompile(`\$(ARGUMENTS|[1-9])\b`)

// isInlineBashDirective reports whether a "!"-led line is a fully-closed single-line !`...` directive —
// the only shape Claude Code actually executes (code.claude.com/docs/en/skills.md); a bare "!cmd" never runs.
func isInlineBashDirective(line string) bool {
	return strings.HasPrefix(line, "!`") && strings.HasSuffix(line, "`") && len(line) > 2
}

// checkCommandScripts walks commands/**/*.md under root, applying the three guard rules below.
// Returns a slice of "<rel>:<line>: <message>" failures, or nil if the file is clean.
func checkCommandScripts(root, path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, path)
	text := string(body)
	var failures []string

	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "!") {
			continue
		}
		lineNo := i + 1
		if !isInlineBashDirective(trimmed) {
			failures = append(failures, rel+":"+strconv.Itoa(lineNo)+": "+trimmed+" is not a single-line !`...` directive")
			continue
		}
		if argPlaceholderPattern.MatchString(trimmed) {
			failures = append(failures, rel+":"+strconv.Itoa(lineNo)+": "+trimmed+" uses $ARGUMENTS/$1-$9, which Claude Code never substitutes inside !`...`")
		}
	}

	// scripts/<x>.sh may be referenced anywhere in the file (a ! line or prose telling the model to run it).
	for _, ref := range scriptRefPattern.FindAllString(text, -1) {
		scriptPath := filepath.Join(root, ref)
		info, serr := os.Stat(scriptPath)
		if serr != nil {
			failures = append(failures, rel+": references "+ref+" which does not exist")
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			failures = append(failures, rel+": references "+ref+" which is not executable")
		}
	}
	return failures, nil
}

// TestCommandBashDirectivesDelegateToScripts guards the commands/*.md "!" directive contract: every
// "!"-led line must be a single-line !`...` directive with no $ARGUMENTS/$1-$9 placeholder (neither
// substitutes), and every scripts/<name>.sh referenced anywhere in the file must exist and be executable.
func TestCommandBashDirectivesDelegateToScripts(t *testing.T) {
	root := repoRoot(t)
	commandsDir := filepath.Join(root, "commands")
	if _, err := os.Stat(commandsDir); err != nil {
		t.Skip("no commands/ dir — not the harness checkout")
	}

	checked := 0
	err := filepath.Walk(commandsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		checked++
		failures, cerr := checkCommandScripts(root, path)
		if cerr != nil {
			return cerr
		}
		for _, f := range failures {
			t.Error(f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Skip("no commands/*.md files found — not the harness checkout")
	}
}

// TestCommandBashDirectiveGuardHasTeeth proves the guard rejects the exact regression it exists to
// catch: a !`...` directive that passes $ARGUMENTS, which Claude Code never substitutes.
func TestCommandBashDirectiveGuardHasTeeth(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte("# bad\n\n!`bash x.sh \"$ARGUMENTS\"`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failures, err := checkCommandScripts(root, bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) == 0 {
		t.Fatal("expected a failure for a !`...` directive using $ARGUMENTS, got none")
	}
}

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
		if err := scanForStaleRefs(t, filepath.Join(root, dir), removed); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// scanForStaleRefs reports (via t.Errorf) every file under scanDir that mentions one of the removed paths.
func scanForStaleRefs(t *testing.T, scanDir string, removed []string) error {
	t.Helper()
	return filepath.Walk(scanDir, func(path string, info os.FileInfo, err error) error {
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
}

// checkAgentFiles checks the file contract against an explicitly supplied required roster.
func checkAgentFiles(dir string, required []string) []string {
	agents, err := DiscoverAgents(dir)
	if err != nil {
		return []string{err.Error()}
	}
	var defects []string
	seen := make(map[string]bool, len(agents))
	for _, agent := range agents {
		seen[agent.dirName] = true
		defects = append(defects, checkAgentFileSource(agent)...)
	}
	for _, name := range required {
		if !seen[name] {
			defects = append(defects, name+".md: required leaf missing")
		}
	}
	return defects
}

func checkAgentFileSource(agent Source) []string {
	var defects []string
	file := agent.dirName + ".md"
	if agent.frontmatter["name"] != agent.dirName {
		defects = append(defects, fmt.Sprintf("%s: name = %v, want %q", file, agent.frontmatter["name"], agent.dirName))
	}
	tools := agentFileTools(agent.frontmatter["tools"])
	external := []string{"codex", "copilot", "gemini", "opencode"}
	leaves := []string{"claude", "codex", "copilot", "gemini", "opencode", "coworker"}
	if slices.Contains(external, agent.dirName) && !slices.Contains(tools, "Bash") {
		defects = append(defects, file+": external leaf tools must include Bash")
	}
	if slices.Contains(leaves, agent.dirName) && slices.Contains(tools, "Agent") {
		defects = append(defects, file+": leaf tools must not include Agent")
	}
	return defects
}

func agentFileTools(value any) []string {
	if values, ok := value.([]any); ok {
		var tools []string
		for _, item := range values {
			if name, ok := item.(string); ok {
				tools = append(tools, name)
			}
		}
		value = tools
	}
	return toolList(value)
}

func TestAgentIDsMatchLiteralLeaves(t *testing.T) {
	got := agentid.All()
	want := []string{"claude", "codex", "copilot", "gemini", "opencode"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("agentid.All() = %v, want %v", got, want)
	}
}

func TestAgentFilesTie(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "agents")
	agents, err := DiscoverAgents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) == 0 {
		t.Fatal("no agent files found in " + dir)
	}
	required := []string{"claude", "codex", "copilot", "gemini", "opencode"}
	if defects := checkAgentFiles(dir, required); len(defects) != 0 {
		t.Fatal(strings.Join(defects, "\n"))
	}
}

func TestAgentFileCheckerHasTeeth(t *testing.T) {
	cases := []struct {
		name, file, declared, tools, want string
	}{
		{name: "good"},
		{"name mismatch", "claude", "wrong", "[Read, Bash]", "claude.md"},
		{"missing required", "copilot", "", "", "copilot.md"},
		{"codex without Bash", "codex", "codex", "[Read]", "codex.md"},
		{"copilot without Bash", "copilot", "copilot", "Read", "copilot.md"},
		{"gemini without Bash", "gemini", "gemini", "[Read]", "gemini.md"},
		{"opencode without Bash", "opencode", "opencode", "Read", "opencode.md"},
		{"claude with Agent", "claude", "claude", "[Read, Bash, Agent]", "claude.md"},
		{"codex with Agent", "codex", "codex", "Read, Bash, Agent", "codex.md"},
		{"copilot with Agent", "copilot", "copilot", "[Read, Bash, Agent]", "copilot.md"},
		{"gemini with Agent", "gemini", "gemini", "Read, Bash, Agent", "gemini.md"},
		{"opencode with Agent", "opencode", "opencode", "[Read, Bash, Agent]", "opencode.md"},
		{"coworker with Agent", "coworker", "coworker", "Read, Bash, Agent", "coworker.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAgentFileFixtures(t, dir)
			if tc.file != "" {
				path := filepath.Join(dir, tc.file+".md")
				if tc.declared == "" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					writeAgentFileFixture(t, path, tc.declared, tc.tools)
				}
			}
			required := []string{"claude", "codex", "copilot", "gemini", "opencode"}
			assertAgentFileDefects(t, checkAgentFiles(dir, required), tc.want)
		})
	}
}

func writeAgentFileFixtures(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		writeAgentFileFixture(t, filepath.Join(dir, name+".md"), name, "[Read, Bash]")
	}
}

func writeAgentFileFixture(t *testing.T, path, name, tools string) {
	t.Helper()
	projectWriteFile(t, path, fmt.Sprintf("---\nname: %s\ndescription: test leaf\ntools: %s\n---\n", name, tools))
}

func assertAgentFileDefects(t *testing.T, defects []string, want string) {
	t.Helper()
	if want == "" {
		if len(defects) != 0 {
			t.Fatalf("good agent files have defects: %v", defects)
		}
		return
	}
	if !strings.Contains(strings.Join(defects, "\n"), want) {
		t.Fatalf("defects = %v, want a defect naming %s", defects, want)
	}
}

func TestAgentFileToolsRepresentations(t *testing.T) {
	for _, value := range []any{
		"Read, Bash",
		[]string{"Read", " Bash "},
		[]any{"Read", " Bash "},
	} {
		if got := agentFileTools(value); !slices.Equal(got, []string{"Read", "Bash"}) {
			t.Errorf("agentFileTools(%#v) = %v, want [Read Bash]", value, got)
		}
	}
}
