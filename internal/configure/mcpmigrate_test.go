package configure

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oldClaudeJSON = `{
  "mcpServers": {
    "web_search": {"type": "stdio", "command": "/bin/claude-code", "args": ["web", "search"]},
    "other": {"command": "x", "args": ["web", "serve"]}
  },
  "projects": {"/p": {"mcpServers": {"web_search": {"command": "/bin/claude-code", "args": ["web","search"]}}}}
}`
	oldCodexTOML = `[mcp_servers.web_search]
command = "/bin/claude-code"
args = ["web", "search"]

[mcp_servers.unrelated]
command = "other-tool"
args = ["web", "search"]
`
	oldOpenCode = `{"mcp": {"web_search": {"type": "local", "command": ["/bin/claude-code", "web", "search"]}}}`
)

// migrateHome points HOME and CLAUDE_CONFIG_DIR at scratch dirs and returns the three config paths.
func migrateHome(t *testing.T) (claudeJSON, codex, opencode string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return filepath.Join(cfg, ".claude.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".config", "opencode", "opencode.json")
}

func writeMigrateFile(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func readMigrateFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMigrateWebSearchArgsRewritesAllThreeShapes(t *testing.T) {
	cj, codex, oc := migrateHome(t)
	writeMigrateFile(t, cj, oldClaudeJSON, 0o600)
	writeMigrateFile(t, codex, oldCodexTOML, 0o600)
	writeMigrateFile(t, oc, oldOpenCode, 0o600)

	var out bytes.Buffer
	if err := MigrateWebSearchArgs(false, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(out.String()), "\n") + 1; n != 3 {
		t.Errorf("want one line per changed file (3), got %d:\n%s", n, out.String())
	}

	got := readMigrateFile(t, cj)
	if strings.Count(got, `"network", "websearch"`)+strings.Count(got, `"network","websearch"`) != 2 {
		t.Errorf("claude.json: user + project registrations must both migrate:\n%s", got)
	}
	if !strings.Contains(got, `"args": ["web", "serve"]`) {
		t.Errorf("claude.json: unrelated args touched:\n%s", got)
	}
	if !strings.Contains(got, `"web_search": {"type": "stdio"`) {
		t.Errorf("claude.json: server name or formatting changed:\n%s", got)
	}

	tomlGot := readMigrateFile(t, codex)
	if !strings.Contains(tomlGot, "[mcp_servers.web_search]\ncommand = \"/bin/claude-code\"\nargs = [\"network\", \"websearch\"]") {
		t.Errorf("codex: claude-code table not migrated:\n%s", tomlGot)
	}
	if !strings.Contains(tomlGot, "[mcp_servers.unrelated]\ncommand = \"other-tool\"\nargs = [\"web\", \"search\"]") {
		t.Errorf("codex: a table not running claude-code must stay untouched:\n%s", tomlGot)
	}

	if want := `["/bin/claude-code", "network", "websearch"]`; !strings.Contains(readMigrateFile(t, oc), want) {
		t.Errorf("opencode: want %s in:\n%s", want, readMigrateFile(t, oc))
	}
}

func TestMigrateWebSearchArgsIdempotent(t *testing.T) {
	cj, codex, oc := migrateHome(t)
	writeMigrateFile(t, cj, oldClaudeJSON, 0o600)
	writeMigrateFile(t, codex, oldCodexTOML, 0o600)
	writeMigrateFile(t, oc, oldOpenCode, 0o600)
	if err := MigrateWebSearchArgs(false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	before := [3]string{readMigrateFile(t, cj), readMigrateFile(t, codex), readMigrateFile(t, oc)}
	var out bytes.Buffer
	if err := MigrateWebSearchArgs(false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("second run printed %q, want nothing", out.String())
	}
	after := [3]string{readMigrateFile(t, cj), readMigrateFile(t, codex), readMigrateFile(t, oc)}
	if before != after {
		t.Error("second run changed a file")
	}
}

func TestMigrateWebSearchArgsAbsentFilesNoop(t *testing.T) {
	cj, codex, oc := migrateHome(t)
	var out bytes.Buffer
	if err := MigrateWebSearchArgs(false, &out); err != nil {
		t.Fatalf("absent files must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q for absent files", out.String())
	}
	for _, p := range []string{cj, codex, oc} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}

func TestMigrateWebSearchArgsKeepsPermsAndDryRunWritesNothing(t *testing.T) {
	cj, _, _ := migrateHome(t)
	writeMigrateFile(t, cj, oldClaudeJSON, 0o600)
	var out bytes.Buffer
	if err := MigrateWebSearchArgs(true, &out); err != nil {
		t.Fatal(err)
	}
	if readMigrateFile(t, cj) != oldClaudeJSON {
		t.Error("--dry-run modified the file")
	}
	if !strings.Contains(out.String(), "would migrate") {
		t.Errorf("dry-run line = %q, want 'would migrate'", out.String())
	}
	if err := MigrateWebSearchArgs(false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(cj)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600", fi.Mode().Perm())
	}
	if !strings.Contains(readMigrateFile(t, cj), "network") {
		t.Error("real run did not migrate")
	}
}

func TestMigrateWebSearchArgsLeavesNewRegistrationAlone(t *testing.T) {
	cj, _, _ := migrateHome(t)
	const cur = `{"mcpServers":{"web_search":{"command":"/bin/claude-code","args":["network","websearch"]}}}`
	writeMigrateFile(t, cj, cur, 0o600)
	var out bytes.Buffer
	if err := MigrateWebSearchArgs(false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || readMigrateFile(t, cj) != cur {
		t.Errorf("already-migrated file touched: out=%q", out.String())
	}
}

const realClaudeJSON = `{
  "numStartups": 3,
  "mcpServers": {
    "web_search": {
      "type": "stdio",
      "command": "/home/u/.claude/bin/claude-code",
      "args": [
        "web",
        "search"
      ],
      "env": {}
    },
    "npx_search": {
      "type": "stdio",
      "command": "npx",
      "args": ["web", "search"]
    }
  }
}
`

func TestMigrateClaudeJSONOnlyClaudeCodeServers(t *testing.T) {
	cj, _, _ := migrateHome(t)
	writeMigrateFile(t, cj, realClaudeJSON, 0o600)
	if err := MigrateWebSearchArgs(false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(realClaudeJSON, "\"web\",\n        \"search\"", "\"network\",\n        \"websearch\"", 1)
	if got := readMigrateFile(t, cj); got != want {
		t.Errorf("want only the claude-code multi-line args migrated, formatting kept:\n%s", got)
	}
}

func TestMigrateCodexNegativeAndIndented(t *testing.T) {
	_, codex, _ := migrateHome(t)
	const untouched = "[mcp_servers.a]\ncommand = \"npx\"\n# claude-code is mentioned here\nargs = [\"web\", \"search\"]\n\n" +
		"[mcp_servers.b]\ncommand = \"tool\"\nargs = [\"web\", \"search\"]\nenv = { WRAP = \"claude-code\" }\n"
	writeMigrateFile(t, codex, untouched, 0o600)
	var out bytes.Buffer
	if err := MigrateWebSearchArgs(false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || readMigrateFile(t, codex) != untouched {
		t.Errorf("table not running claude-code was touched: out=%q\n%s", out.String(), readMigrateFile(t, codex))
	}
	writeMigrateFile(t, codex, "[mcp_servers.web_search]\ncommand = \"claude-code\"\n  args = [\"web\", \"search\"]\n", 0o600)
	if err := MigrateWebSearchArgs(false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := readMigrateFile(t, codex); !strings.Contains(got, `  args = ["network", "websearch"]`) {
		t.Errorf("indented args not migrated:\n%s", got)
	}
}

func TestMigrateOpenCodeNotClaudeCodeUntouched(t *testing.T) {
	_, _, oc := migrateHome(t)
	const body = `{"mcp": {"web_search": {"type": "local", "command": ["/bin/not-claude-code", "web", "search"]}}}`
	writeMigrateFile(t, oc, body, 0o600)
	var out bytes.Buffer
	if err := MigrateWebSearchArgs(false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || readMigrateFile(t, oc) != body {
		t.Errorf("/bin/not-claude-code rewritten: out=%q\n%s", out.String(), readMigrateFile(t, oc))
	}
}
