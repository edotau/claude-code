package hooks

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBashGuardIgnoresHeredocBody(t *testing.T) {
	cases := []struct {
		name, command string
		want          int
	}{
		{"body", `cat >> /tmp/claude-1000/p6b/notes.md <<'EOF'
### note
- evidence: the command used sudo apt install, which the gate refuses
EOF`, ExitProceed},
		{"sudo", "sudo rm -rf /tmp/x", ExitBlock},
		{"after", "cat <<EOF\nhello\nEOF\nsudo ls", ExitBlock},
		{"before", "rm -rf /\ncat <<EOF\nhello\nEOF", ExitBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"session_id": "s", "cwd": "/tmp", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": tc.command}})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := BashGuard(bytes.NewReader(payload), &stdout, &stderr)
			if code != tc.want || (tc.want == ExitProceed && stderr.Len() != 0) {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, tc.want, stderr.String())
			}
		})
	}
}
