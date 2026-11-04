package configure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A web_search table whose last line has no newline is replaced whole, never left as a duplicate key.
func TestRegisterCodexMCPUnterminatedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	old := "[mcp_servers.web_search]\ncommand = \"x\"\nargs = [\"network\", \"websearch\"]"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegisterCodexMCP(path, "/bin/claude-code"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if n := strings.Count(string(got), "args ="); n != 1 {
		t.Fatalf("want one args line, got %d:\n%s", n, got)
	}
}

// A bare base64 value whose padding follows an uppercase run is a value, not NAME=value.
func TestParseAssignmentBarePadding(t *testing.T) {
	if name, v := ParseAssignment("ABCD12=="); name != "" || v != "ABCD12==" {
		t.Fatalf("got (%q, %q)", name, v)
	}
}
