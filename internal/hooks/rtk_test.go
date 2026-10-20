package hooks

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func rtkOut(t *testing.T, in string) (cmd string, emitted bool) {
	t.Helper()
	var sb strings.Builder
	if rc := BashGuard(strings.NewReader(in), &sb, io.Discard); rc != ExitProceed {
		t.Fatalf("rc = %d, want ExitProceed (%d) for this safe command", rc, ExitProceed)
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", false
	}
	var out struct {
		HookSpecificOutput struct {
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &out); err != nil {
		t.Fatalf("emitted non-JSON: %v\n%s", err, sb.String())
	}
	s, _ := out.HookSpecificOutput.UpdatedInput["command"].(string)
	return s, true
}

func rtkBashIn(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 120},
	})
	return string(b)
}

func TestRTKWrapsEligibleCommand(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo") // stand-in for an installed rtk
	got, ok := rtkOut(t, rtkBashIn("git log --oneline -20"))
	if !ok || got != "echo git log --oneline -20" {
		t.Fatalf("command = %q (emitted=%v), want the wrapped form", got, ok)
	}
}

func TestRTKMachineReadableFlagsMatchTokens(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	for _, cmd := range []string{
		"git checkout feature-zero",
		`git commit -m "add -z flag"`,
		"git checkout feature--json",
		"git log --formatter=short",
		"kubectl get pods -o wide json",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); !ok || got != "echo "+cmd {
			t.Errorf("%q: got %q (emitted=%v), want rewrite", cmd, got, ok)
		}
	}
	for _, flag := range rtkMachineReadable {
		variants := []string{flag, strings.ToUpper(flag)}
		if flag != "-o json" {
			variants = append(variants, flag+"=value")
		}
		for _, suffix := range variants {
			if got, ok := rtkOut(t, rtkBashIn("git log "+suffix)); ok {
				t.Errorf("real flag %q was wrapped to %q", suffix, got)
			}
		}
	}
	if got, ok := rtkOut(t, rtkBashIn(`kubectl get pods -o "json"`)); ok {
		t.Errorf("quoted JSON output was wrapped to %q", got)
	}
}

// updatedInput REPLACES the whole record, so every sibling key must survive the rewrite.
func TestRTKPreservesOtherInputKeys(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	var sb strings.Builder
	BashGuard(strings.NewReader(rtkBashIn("npm install")), &sb, io.Discard)
	var out struct {
		HookSpecificOutput struct {
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	json.Unmarshal([]byte(sb.String()), &out)
	if out.HookSpecificOutput.UpdatedInput["description"] != "d" {
		t.Errorf("description dropped: %#v", out.HookSpecificOutput.UpdatedInput)
	}
	if out.HookSpecificOutput.UpdatedInput["timeout"] == nil {
		t.Errorf("timeout dropped: %#v", out.HookSpecificOutput.UpdatedInput)
	}
}

// The whole point of the guard: this harness's own hooks and workflows parse porcelain/JSON output.
// Compressing it would silently corrupt every parser downstream.
func TestRTKNeverWrapsMachineReadableOutput(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	for _, cmd := range []string{
		"git status --porcelain",
		"git diff --name-only",
		"git diff --numstat -M HEAD",
		"git log --format=%H",
		"git ls-files -z",
		"npm ls --json",
		"cargo metadata --format-version 1",
		"kubectl get pods -o json",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — machine-readable output must bypass rtk", cmd, got)
		}
	}
}

// The flag list cannot catch these: `git rev-parse HEAD` carries no machine-readable flag, yet its
// one line is parsed verbatim by callers. Compressing a VALUE is the same corruption as compressing
// porcelain — the subcommand, not a flag, is what identifies it.
func TestRTKNeverWrapsValueProducingSubcommands(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	for _, cmd := range []string{
		"git rev-parse HEAD",
		"git rev-parse --abbrev-ref HEAD",
		"git config --get user.email",
		"git describe --tags",
		"git ls-files",
		"git symbolic-ref refs/remotes/origin/HEAD",
		"git merge-base master HEAD",
		"git show-ref refs/heads/master",
		"git cat-file -p HEAD",
		"git branch --show-current",
		// Global options precede the subcommand. Classifying on fields[1] alone missed every one of
		// these, so their values reached the compressor — the exact corruption this guard exists to
		// prevent, and both forms appear in the harness's own scripts and hooks.
		"git -C /repo rev-parse HEAD",
		"git --no-pager config --get user.email",
		"git -c core.pager=cat rev-parse --abbrev-ref HEAD",
		"git --git-dir=/repo/.git rev-parse HEAD",
		"git -C /repo --no-pager ls-files",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — its output is a value to parse, not a report to read", cmd, got)
		}
	}
}

// An unrecognized global flag makes gitRTKSubcommand return "" (documented "fail closed"); rtkEligible
// must treat that the same as a known value subcommand and skip wrapping, not fall through to wrap.
func TestRTKNeverWrapsUnrecognizedGlobalFlag(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	for _, cmd := range []string{
		"git --unknown-global-flag rev-parse HEAD",
		"git -Z rev-parse HEAD",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — an unrecognized global flag must fail closed, not wrap", cmd, got)
		}
	}
}

// Eligibility is judged on the FIRST word, but the rewrite prefixes the whole string. In a chain the
// later segments were never checked, and the "output is summarized" notice then misdescribes output
// that partly did not pass through rtk at all.
func TestRTKNeverWrapsCommandChains(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	for _, cmd := range []string{
		"git log --oneline -5 && ls",
		"git log -1; cat README.md",
		"git fetch || git remote -v",
		"git log --oneline\ncat go.mod",
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q — only the first segment was ever checked for eligibility", cmd, got)
		}
	}
}

// A pipe is not a chain: rtk's compressed stdout is what the caller asked the filter to read, and a
// single eligible command feeding `head`/`grep` is the common case the compressor exists for.
func TestRTKStillWrapsPipelines(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	if got, ok := rtkOut(t, rtkBashIn("git log --oneline | head -20")); !ok {
		t.Errorf("pipeline was not wrapped (got %q); a pipe does not start a new command", got)
	}
}

func TestRTKSkipsIneligibleAndAlreadyWrapped(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	for _, cmd := range []string{
		"ls -la",                // not an eligible command
		"echo git status",       // eligible name is not the first word
		"echo git log",          // already the binary itself: never double-wrap
		"cd /tmp && git status", // first word is cd, so the wrap would not apply to git anyway
		"",                      // nothing to do
	} {
		if got, ok := rtkOut(t, rtkBashIn(cmd)); ok {
			t.Errorf("%q was wrapped to %q, want no rewrite", cmd, got)
		}
	}
}

func TestRTKMissingBinaryIsNoOp(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "definitely-not-a-real-binary-xyz")
	if got, ok := rtkOut(t, rtkBashIn("git log")); ok {
		t.Errorf("missing binary still wrapped to %q — must degrade to a no-op", got)
	}
}

func TestRTKIgnoresNonBashTools(t *testing.T) {
	t.Setenv(rtkBinaryEnv, "echo")
	b, _ := json.Marshal(map[string]any{
		"tool_name": "Read", "tool_input": map[string]any{"command": "git log"},
	})
	if got, ok := rtkOut(t, string(b)); ok {
		t.Errorf("non-Bash tool wrapped to %q", got)
	}
}

func TestRTKCommandList(t *testing.T) {
	if got, want := RTKCommandList(), "cargo docker git kubectl npm pytest"; got != want {
		t.Errorf("RTKCommandList() = %q, want %q", got, want)
	}
}
