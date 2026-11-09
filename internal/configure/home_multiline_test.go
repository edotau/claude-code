package configure

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexWiredIgnoresMultilineStrings(t *testing.T) {
	for _, delimiter := range []string{`"""`, `'''`} {
		for _, tc := range []struct {
			name, command, args string
			wired               bool
		}{
			{"missing args", "/opt/claude-code", "[]", false},
			{"missing command", "/opt/other", `["network", "websearch"]`, false},
			{"active after string", "/opt/claude-code", `["network", "websearch"]`, true},
		} {
			t.Run(delimiter+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.toml")
				body := "[mcp_servers.web_search]\nnotes = " + delimiter + "\n" +
					"command = \"/opt/claude-code\"\nargs = [\"network\", \"websearch\"]\n" + delimiter + "\n" +
					"command = \"" + tc.command + "\"\nargs = " + tc.args + "\n"
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if got := CodexWired(path); got != tc.wired {
					t.Fatalf("CodexWired = %v, want %v for:\n%s", got, tc.wired, body)
				}
			})
		}
	}
}
