package agent_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/cli"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vendors"
)

// std2Run runs `claude-code <argv>` in-process and returns exit code, stdout and stderr.
func std2Run(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := cli.Run(append([]string{"claude-code"}, argv...))
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return code, string(o), string(e)
}

// std2Env is a hermetic config dir + HOME with the agy login marker, so every fake CLI looks logged in.
func std2Env(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "test-key") // an own-login route for opencode
	marker := vendors.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// std2Fakes installs a stand-in child per CLI agent (copilot rides the SDK and is covered in resume_std2_test.go).
func std2Fakes(t *testing.T) {
	t.Helper()
	pathstest.FakeVendor(t, "claude", "cat > /dev/null; sleep 0.05\n"+
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sc","model":"fake-claude"}'`+"\n"+
		`printf '%s\n' '{"type":"assistant","message":{"model":"fake-claude","content":[{"type":"text","text":"hello"}]}}'`+"\n"+
		`printf '%s\n' '{"type":"result","result":"hello","usage":{"input_tokens":3,"output_tokens":2}}'`)
	pathstest.FakeVendor(t, "codex", `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\n"+"cat > /dev/null; sleep 0.05; echo hello")
	pathstest.FakeVendor(t, "agy", "cat > /dev/null; sleep 0.05\n"+
		`printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hello"}}'`+"\n"+
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"hello","num_turns":1,"usage":{"input_tokens":3,"output_tokens":2}}}'`)
	pathstest.FakeVendor(t, "opencode", "sleep 0.05\n"+
		`printf '%s\n' '{"type":"text","sessionID":"so","part":{"text":"hello"}}'`+"\n"+
		`printf '%s\n' '{"type":"step_finish","sessionID":"so","part":{"tokens":{"Input":3,"Output":2}}}'`)
}

var std2Profile = map[string]string{"claude": "", "codex": "chatgpt", "gemini": "", "opencode": ""}

func std2Envelope(t *testing.T, out string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

// Every runner's --format json success emits the one standard envelope.
func TestStd2EnvelopeContractSuccess(t *testing.T) {
	noUsage := map[string]bool{"codex": true}
	for _, name := range []string{"claude", "codex", "gemini", "opencode"} {
		t.Run(name, func(t *testing.T) {
			std2Env(t)
			std2Fakes(t)
			code, out, stderr := std2Run(t, "ask", "--agent", name, "--profile", std2Profile[name], "--fallback", "none", "--native", "--format", "json", "say hello")
			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
			}
			env := std2Envelope(t, out)
			if env["schema"] != float64(1) {
				t.Errorf("schema = %v, want 1", env["schema"])
			}
			if env["status"] != "ok" {
				t.Errorf("status = %v, want ok", env["status"])
			}
			if env["agent"] != name {
				t.Errorf("agent = %v, want %s", env["agent"], name)
			}
			// opencode on its own login picks its own model and reports none; the key must still be present.
			if m, ok := env["model"].(string); !ok || (m == "" && name != "opencode") {
				t.Errorf("model = %v, want non-empty", env["model"])
			}
			if id, _ := env["dispatch_id"].(string); id == "" {
				t.Errorf("dispatch_id = %v, want non-empty", env["dispatch_id"])
			}
			if v, ok := env["fallback"]; !ok || v != nil {
				t.Errorf("fallback = %v (present=%v), want explicit null", v, ok)
			}
			if d, _ := env["duration_ms"].(float64); d <= 0 {
				t.Errorf("duration_ms = %v, want > 0", env["duration_ms"])
			}
			if noUsage[name] {
				if v, ok := env["usage"]; !ok || v != nil {
					t.Errorf("usage = %v (present=%v), want explicit null: %s reports none", v, ok, name)
				}
			} else if _, ok := env["usage"].(map[string]any); !ok {
				t.Errorf("usage = %v, want an object", env["usage"])
			}
		})
	}
}

// An unknown --profile fails the same way on codex and opencode: status error, error {kind,message}, exit 1.
func TestStd2EnvelopeErrorShape(t *testing.T) {
	shapes := map[string][]string{}
	for _, name := range []string{"codex", "opencode"} {
		t.Run(name, func(t *testing.T) {
			std2Env(t)
			std2Fakes(t)
			code, out, stderr := std2Run(t, "ask", "--agent", name, "--profile", "no-such-profile", "--fallback", "none", "--format", "json", "hi")
			if code != 1 {
				t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
			}
			env := std2Envelope(t, out)
			if env["status"] != "error" {
				t.Errorf("status = %v, want error", env["status"])
			}
			e, ok := env["error"].(map[string]any)
			if !ok {
				t.Fatalf("error = %#v, want an object {kind,message}", env["error"])
			}
			if k, _ := e["kind"].(string); k == "" {
				t.Errorf("error.kind = %v, want non-empty", e["kind"])
			}
			if m, _ := e["message"].(string); m == "" {
				t.Errorf("error.message = %v, want non-empty", e["message"])
			}
			var keys []string
			for k := range env {
				keys = append(keys, k)
			}
			shapes[name] = keys
		})
	}
	a, b := shapes["codex"], shapes["opencode"]
	if len(a) != len(b) {
		t.Errorf("codex and opencode error envelopes differ in keys: %v vs %v", a, b)
	}
}

// std2Marker is a fake child that records its invocation (never a `login` probe) by touching a file.
func std2Marker(t *testing.T, vendor, body string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), vendor+".ran")
	pathstest.FakeVendor(t, vendor, `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\n"+
		"touch "+marker+"\n"+body)
	return marker
}

// A resume whose launch fails returns an error and never runs another agent's runner.
func TestStd2ResumeNeverFallsBack(t *testing.T) {
	std2Env(t)
	codexRan := std2Marker(t, "codex", "echo 'codex: boom' >&2; exit 1")
	others := map[string]string{
		"claude":   std2Marker(t, "claude", "exit 1"),
		"agy":      std2Marker(t, "agy", "exit 1"),
		"opencode": std2Marker(t, "opencode", "exit 1"),
	}
	code, out, stderr := std2Run(t, "codex", "resume", "sess-1", "continue please")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero: the resumed session's launch failed\nstdout: %s", out)
	}
	if _, err := os.Stat(codexRan); err != nil {
		t.Fatalf("the codex runner was never reached (%v); the test proves nothing\nstderr: %s", err, stderr)
	}
	for name, marker := range others {
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("resume fell back and ran %s; a session belongs to one agent\nstderr: %s", name, stderr)
		}
	}
}

// --profile given to resume reaches the runner's target resolution instead of being dropped.
func TestStd2ResumeForwardsProfile(t *testing.T) {
	std2Env(t)
	std2Marker(t, "codex", "echo ok")
	code, out, stderr := std2Run(t, "codex", "resume", "sess-1", "--profile", "no-such-profile", "continue")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for an unknown --profile\nstdout: %s", out)
	}
	if !strings.Contains(stderr+out, "no-such-profile") {
		t.Errorf("output never names the --profile given to resume:\nstdout: %s\nstderr: %s", out, stderr)
	}
}

// Every usage failure in --format json mode prints one parseable error envelope and keeps its exit code.
func TestStd2JSONModeFailuresPrintEnvelope(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		code int
	}{
		{"conflicting tools flags", []string{"ask", "--format", "json", "--tools", "--no-tools", "hi"}, 1},
		{"invalid fallback", []string{"ask", "--format", "json", "--fallback", "bogus", "hi"}, 1},
		{"missing prompt", []string{"ask", "--format", "json"}, 2},
		{"retry of unknown id", []string{"ask", "--format", "json", "--retry", "no-such-id"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			std2Env(t)
			std2Fakes(t)
			code, out, stderr := std2Run(t, c.argv...)
			if code == 0 {
				t.Fatalf("exit = 0, want a failure\nstdout: %s", out)
			}
			if c.code == 2 && code != 2 {
				t.Errorf("exit = %d, want 2\nstderr: %s", code, stderr)
			}
			env := std2Envelope(t, out)
			if env["status"] != "error" {
				t.Errorf("status = %v, want error: %s", env["status"], out)
			}
			e, ok := env["error"].(map[string]any)
			if !ok || e["message"] == "" {
				t.Fatalf("error = %#v, want {kind,message}", env["error"])
			}
			if c.name != "retry of unknown id" && e["kind"] != "usage" {
				t.Errorf("error.kind = %v, want usage", e["kind"])
			}
		})
	}
}

// An unknown --profile is a caller error: all five agents report kind usage, not fatal.
func TestStd2UnknownProfileKindUsageAllAgents(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		t.Run(name, func(t *testing.T) {
			std2Env(t)
			std2Fakes(t)
			code, out, stderr := std2Run(t, "ask", "--agent", name, "--profile", "nosuch", "--fallback", "none", "--format", "json", "x")
			if code != 1 {
				t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
			}
			env := std2Envelope(t, out)
			e, _ := env["error"].(map[string]any)
			if e["kind"] != "usage" {
				t.Errorf("error.kind = %v, want usage: %s", e["kind"], out)
			}
		})
	}
}
