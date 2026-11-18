package safety

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

type tamperCase struct {
	name, cmd, agentID, dispatchID string
	want                           core.Decision
}

var tamperCases = []tamperCase{
	{"assign empty id", `HARNESS_DISPATCH_ID= claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"env -u id", `env -u HARNESS_DISPATCH_ID claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"unset permissions", `unset HARNESS_DISPATCH_PERMISSIONS; claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"assign max depth", `HARNESS_DISPATCH_MAX_DEPTH=0 claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"assign permissions", `HARNESS_DISPATCH_PERMISSIONS='{}' claude-code codex ask x`, "a1", "", core.Block},
	{"export depth", `export HARNESS_DISPATCH_DEPTH=0`, "a1", "", core.Block},
	{"env -i before claude-code", `env -i PATH=/usr/bin claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"dispatch leaf unsets", `unset HARNESS_DISPATCH_ID`, "", "d1", core.Block},
	{"env -i before a vendor launcher", `env -i PATH=/usr/bin codex exec x`, "a1", "", core.Block},
	{"env -i before opencode", `env -i opencode run x`, "a1", "", core.Block},
	{"assign after another assignment", `FOO=1 HARNESS_DISPATCH_ID= claude-code agents ask x`, "a1", "", core.Block},
	{"assign through env after -u", `env -u FOO HARNESS_DISPATCH_ID= claude-code agents ask x`, "a1", "", core.Block},
	{"unset inside bash -c", `bash -c "unset HARNESS_DISPATCH_ID; claude-code agents ask x"`, "a1", "", core.Block},
	{"assign inside sh -c", `sh -c 'HARNESS_DISPATCH_DEPTH=0 claude-code agents ask x'`, "a1", "", core.Block},
	{"exec env -u id", `exec env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"nohup env -u id", `nohup env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"time env -u id", `time env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"command env -u id", `command env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"exec env -i launcher", `exec env -i claude-code agents ask x`, "a1", "", core.Block},
	{"nohup env assigns depth", `nohup env HARNESS_DISPATCH_DEPTH=0 claude-code agents ask x`, "a1", "", core.Block},
	{"sudo env -u id", `sudo env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"absolute env -u id", `/usr/bin/env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"time go test", `time go test ./...`, "a1", "", core.Allow},
	{"nohup dispatch", `nohup claude-code agents ask --agent codex "x" &`, "a1", "", core.Allow},
	{"exec env unrelated assign", `exec env FOO=1 claude-code agents ask x`, "a1", "", core.Allow},
	{"command -v env", `command -v env`, "a1", "", core.Allow},
	{"time env piped to sort", `time env | sort`, "a1", "", core.Allow},
	{"nice make", `nice -n 10 make test`, "a1", "", core.Allow},
	{"text only", `echo "exec env -u HARNESS_DISPATCH_ID"`, "a1", "", core.Allow},
	{"argument is no assignment", `go test ./... -args HARNESS_DISPATCH_DEPTH=2`, "a1", "", core.Allow},
	{"plain dispatch", `claude-code agents ask --agent codex x`, "a1", "", core.Allow},
	{"echo id", `echo $HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"printenv depth", `printenv HARNESS_DISPATCH_DEPTH`, "a1", "", core.Allow},
	{"unrelated assign", `FOO=1 claude-code agents ask --agent codex x`, "a1", "", core.Allow},
	{"env -i other command", `env -i PATH=/usr/bin ls`, "a1", "", core.Allow},
	{"assign before env -u", `A=1 env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"sudo env -u", `sudo env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"xargs env -u", `echo x | xargs env -u HARNESS_DISPATCH_ID claude-code agents ask`, "a1", "", core.Block},
	{"find -exec env -u", `find . -exec env -u HARNESS_DISPATCH_ID claude-code agents ask {} \;`, "a1", "", core.Block},
	{"exec -a env -u", `exec -a x env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"then env -u", `if true; then env -u HARNESS_DISPATCH_ID claude-code agents ask x; fi`, "a1", "", core.Block},
	{"else env -u", `if false; then true; else env -u HARNESS_DISPATCH_ID claude-code agents ask x; fi`, "a1", "", core.Block},
	{"do env -u", `for i in 1; do env -u HARNESS_DISPATCH_ID claude-code agents ask x; done`, "a1", "", core.Block},
	{"export -n", `export -n HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"declare +x", `declare +x HARNESS_DISPATCH_DEPTH`, "a1", "", core.Block},
	{"typeset +x", `typeset +x HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"env -i sh -c claude-code", `env -i sh -c 'claude-code agents ask x'`, "a1", "", core.Block},
	{"env -i claude-code", `env -i claude-code agents ask x`, "a1", "", core.Block},
	{"env dash claude-code", `env - claude-code agents ask x`, "a1", "", core.Block},
	{"env -u other var", `env -u SOME_OTHER_VAR claude-code agents ask x`, "a1", "", core.Allow},
	{"env -i make", `env -i make`, "a1", "", core.Allow},
	{"find exec grep id", `find . -exec grep HARNESS_DISPATCH_ID {} \;`, "a1", "", core.Allow},
	{"grep -rn id", `grep -rn HARNESS_DISPATCH_ID .`, "a1", "", core.Allow},
	{"export other", `export FOO=1`, "a1", "", core.Allow},
	{"commit message mentions unset", `git commit -m 'docs: unset HARNESS_DISPATCH_ID is blocked'`, "a1", "", core.Allow},
	{"heredoc mentions unset", "cat > notes.txt <<'EOF'\nunset HARNESS_DISPATCH_ID\nenv -u HARNESS_DISPATCH_ID x\nEOF", "a1", "", core.Allow},
	{"builtin unset", `builtin unset HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"command unset", `command unset HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"unquoted eval unset", `eval unset HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"brace list unset", `unset HARNESS_DISPATCH_{ID,DEPTH}`, "a1", "", core.Block},
	{"brace list env -u", `env -u HARNESS_DISPATCH_{ID,DEPTH} claude-code agents ask x`, "a1", "", core.Block},
	{"su -c unset", `su -c 'unset HARNESS_DISPATCH_ID; claude-code agents ask x' root`, "a1", "", core.Block},
	{"script -qc unset", `script -qc 'unset HARNESS_DISPATCH_ID; claude-code agents ask x' /dev/null`, "a1", "", core.Block},
	{"indirect unset", `N=HARNESS_DISPATCH_ID; unset $N`, "a1", "", core.Block},
	{"indirect quoted export -n", `N="HARNESS_DISPATCH_DEPTH"; export -n ${N}`, "a1", "", core.Block},
	{"indirect env -u", `N=HARNESS_DISPATCH_ID && env -u $N claude-code agents ask x`, "a1", "", core.Block},
	{"heredoc into bash", "bash <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"heredoc into sh -s", "sh -s <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"heredoc piped to sh", "cat <<EOF | sh\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"heredoc written then run", "cat > s.sh <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nbash s.sh", "a1", "", core.Block},
	{"commit heredoc body", "git commit -F - <<'EOF'\nfix: x\n\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"bare cat heredoc body", "cat <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"commit message substitution heredoc", "git commit -m \"$(cat <<'EOF'\nfix: x\n\nunset HARNESS_DISPATCH_ID\nEOF\n)\"", "a1", "", core.Allow},
	{"su other user", `su - otheruser`, "a1", "", core.Allow},
	{"script no -c", `script -q /dev/null ls`, "a1", "", core.Allow},
	{"alias unused", `N=HARNESS_DISPATCH_ID; echo $N`, "a1", "", core.Allow},
	{"env -u other command", `env -u FOO claude-code agents status`, "a1", "", core.Allow},
	{"echo unset", `echo unset HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"command -v unset", `command -v unset`, "a1", "", core.Allow},
	{"main session sets depth", `HARNESS_DISPATCH_MAX_DEPTH=5 claude-code agents ask --agent codex x`, "", "", core.Allow},
}

// A leaf may not assign, export, unset or clear a HARNESS_DISPATCH_* variable; the main session may.
func TestDispatchTamper(t *testing.T) {
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	for _, c := range tamperCases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HARNESS_DISPATCH_ID", c.dispatchID)
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": "/tmp", "agent_id": c.agentID,
				"tool_input": map[string]string{"command": c.cmd}})
			if r := Scan(core.ParseInput(bytes.NewReader(payload))); r.Decision != c.want {
				t.Fatalf("decision %v, want %v: %s", r.Decision, c.want, r.Reason)
			}
		})
	}
}

// sudo is blocked by its own rule; the tamper check alone must not read an ordinary sudo command as a tamper.
func TestTamperNameIgnoresPlainSudo(t *testing.T) {
	if name := tamperName("sudo ls"); name != "" {
		t.Errorf("tamperName(sudo ls) = %q, want none", name)
	}
}
