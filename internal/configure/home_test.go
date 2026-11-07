package configure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
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

func TestRegisterCodexMCPPreservesOtherTables(t *testing.T) {
	for _, header := range []string{"[mcp_servers.web_search]\n", "[mcp_servers.web_search] # search\r\n"} {
		t.Run(header, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			prefix := "instructions = '''\n[mcp_servers.web_search]\nexample only\n'''\n"
			other := "  [mcp_servers.other]\ncommand = \"other\"\n"
			old := prefix + header + "command = \"old\"\nargs = []\n" + other
			if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := RegisterCodexMCP(path, "/bin/claude-code"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := prefix + "[mcp_servers.web_search]\ncommand = \"/bin/claude-code\"\nargs = [\"network\", \"websearch\"]\n\n" + other
			if string(got) != want {
				t.Fatalf("registration changed unrelated TOML:\ngot: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestRegisterClaudeMCPWrapsExitError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "claude", "printf 'registration failed\\n'; exit 7")
	err := RegisterClaudeMCPContext(context.Background(), "/bin/claude-code")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 || !strings.Contains(err.Error(), "registration failed") {
		t.Fatalf("registration error = %v; want wrapped exit status 7 and command output", err)
	}
}

// A failing add must not remove the existing web_search registration first.
func TestRegisterClaudeMCPAddFailureKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	log := filepath.Join(dir, "calls")
	pathstest.FakeVendor(t, "claude", `echo "$2" >> `+log+`; [ "$2" = add ] && { echo locked; exit 3; }; exit 0`)
	if err := RegisterClaudeMCPContext(context.Background(), "/bin/claude-code"); err == nil {
		t.Fatal("want add error")
	}
	got, _ := os.ReadFile(log)
	if strings.Contains(string(got), "remove") {
		t.Fatalf("existing registration removed before a successful add; calls:\n%s", got)
	}
}
