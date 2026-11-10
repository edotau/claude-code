package configure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
			want := prefix + "[mcp_servers.web_search]\ncommand = \"/bin/claude-code\"\nargs = [\"network\", \"websearch\"]\n" +
				"default_tools_approval_mode = \"approve\"\nenv_vars = [\"DBUS_SESSION_BUS_ADDRESS\", \"XDG_RUNTIME_DIR\"]\n\n" + other
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

func TestCodexWiredIgnoresCommentedArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, tc := range []struct {
		name, args string
		wired      bool
	}{
		{"commented", "args = []\n# args = [\"network\", \"websearch\"]\n", false},
		{"active", "args = [\"network\", \"websearch\"] # enabled\n", true},
		{"other binary", "args = [\"network\", \"websearch\"]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := "/opt/claude-code"
			if tc.name == "other binary" {
				cmd = "/x/other-claude-code-wrapper"
			}
			body := "[mcp_servers.web_search]\ncommand = \"" + cmd + "\"\n" + tc.args
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := CodexWired(path); got != tc.wired {
				t.Fatalf("CodexWired = %v, want %v for:\n%s", got, tc.wired, body)
			}
		})
	}
}

func TestRegisterClaudeMCPRestoresAfterReaddFailure(t *testing.T) {
	for _, mode := range []string{"failure", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("MCP_TEST_CONFIG", paths.ClaudeJSON())
			t.Setenv("MCP_TEST_CALLS", filepath.Join(t.TempDir(), "calls"))
			t.Setenv("MCP_TEST_MODE", mode)
			const original = `{"mcpServers":{"web_search":{"command":"/old/search","args":["custom"],"env":{"SEARCH_MODE":"local"}}}}`
			if err := os.WriteFile(paths.ClaudeJSON(), []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			pathstest.FakeVendor(t, "claude", mcpReaddFailureScript)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := RegisterClaudeMCPContext(ctx, "/bin/claude-code")
			assertMCPReaddError(t, mode, err)
			assertMCPRestored(t, original)
		})
	}
}

const mcpReaddFailureScript = `
case "$2" in
  add)
    if [ ! -f "$MCP_TEST_CALLS" ]; then
      echo add > "$MCP_TEST_CALLS"
      echo 'already exists'
      exit 1
    fi
    echo add >> "$MCP_TEST_CALLS"
    if [ "$MCP_TEST_MODE" = deadline ]; then exec sleep 10; fi
    echo 'replacement failed'
    exit 7
    ;;
  remove)
    echo remove >> "$MCP_TEST_CALLS"
    printf '{"mcpServers":{}}' > "$MCP_TEST_CONFIG"
    ;;
  add-json)
    echo add-json >> "$MCP_TEST_CALLS"
    printf '{"mcpServers":{"web_search":%s}}' "$6" > "$MCP_TEST_CONFIG"
    ;;
  *) exit 9 ;;
esac
`

func assertMCPReaddError(t *testing.T, mode string, err error) {
	t.Helper()
	if mode == "deadline" {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("replacement error = %v, want context deadline", err)
		}
		return
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 || !strings.Contains(err.Error(), "replacement failed") {
		t.Fatalf("replacement error = %v, want wrapped exit 7 and replacement output", err)
	}
}

func assertMCPRestored(t *testing.T, original string) {
	t.Helper()
	calls, err := os.ReadFile(os.Getenv("MCP_TEST_CALLS"))
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "add\nremove\nadd\nadd-json\n" {
		t.Fatalf("replacement did not restore after removal; calls:\n%s", calls)
	}
	raw, err := os.ReadFile(paths.ClaudeJSON())
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(original), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored registration = %s, want %s", raw, original)
	}
}

const mcpBin = "/custom path/claude-code"

var (
	mcpRemoveArgv = []string{"mcp", "remove", "-s", "user", "web_search"}
	mcpAddArgv    = []string{"mcp", "add", "-s", "user", "web_search", "--", mcpBin, "network", "websearch"}
)

func registerMCPMode(t *testing.T, mode string) (string, error) {
	t.Helper()
	_, log, _ := configureCommandEnv(t, mode)
	if strings.HasPrefix(mode, "exists") {
		doc := `{"mcpServers":{"web_search":{"command":"old-search","args":[]}}}`
		if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return log, RegisterClaudeMCPContext(context.Background(), mcpBin)
}

// TestRegisterClaudeMCPContextFresh: a first registration runs add only, never remove.
func TestRegisterClaudeMCPContextFresh(t *testing.T) {
	log, err := registerMCPMode(t, "normal")
	if err != nil {
		t.Fatal(err)
	}
	assertConfigureArgv(t, log, mcpAddArgv)
}

// TestRegisterClaudeMCPContextReplacesExisting: "already exists" from add triggers remove, then add again.
func TestRegisterClaudeMCPContextReplacesExisting(t *testing.T) {
	log, err := registerMCPMode(t, "exists")
	if err != nil {
		t.Fatal(err)
	}
	assertConfigureArgv(t, log, mcpAddArgv, mcpRemoveArgv, mcpAddArgv)
}

// TestRegisterClaudeMCPContextRemoveFails: a failed remove is reported and add is not retried.
func TestRegisterClaudeMCPContextRemoveFails(t *testing.T) {
	log, err := registerMCPMode(t, "exists-remove-fail")
	if err == nil || !strings.Contains(err.Error(), "claude mcp remove") {
		t.Errorf("remove failure = %v, want claude mcp remove error", err)
	}
	assertConfigureArgv(t, log, mcpAddArgv, mcpRemoveArgv)
}

// TestRegisterClaudeMCPContextCancelAdd: cancelling during add returns the context error promptly.
func TestRegisterClaudeMCPContextCancelAdd(t *testing.T) {
	_, log, ready := configureCommandEnv(t, "cancel-add")
	assertConfigureCancellation(t, ready, func(ctx context.Context) error {
		return RegisterClaudeMCPContext(ctx, mcpBin)
	})
	assertConfigureArgv(t, log, mcpAddArgv)
}
