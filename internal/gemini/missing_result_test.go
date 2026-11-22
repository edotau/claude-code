package gemini

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/redact"
)

func TestAgyMissingResultDiagnostics(t *testing.T) {
	hosttest.SkipOnWindows(t, "recorded stream shell replay")
	const secret = "fixture-vault-value-not-a-real-credential"
	redact.StubValues(t, []string{secret})
	const init = `{"event":"init","conversation_id":"recorded-eof"}`
	const success = `{"event":"result","result":{"status":"SUCCESS","response":"ok"}}`
	for _, tc := range []struct{ name, stream, stderr, log, want string }{
		{"structured stderr", init, `AGY_ERROR: {"short_error":"upstream failure ` + secret + `"}`, "", "upstream failure"},
		{"log tail", init, "", strings.Repeat("earlier\n", 8) + strings.Repeat("shutdown\n", 4) + "upstream stopped " + secret, "upstream stopped"},
		{"clipped secret", init, "", strings.Repeat("s", agyLogTailBytes) + "\nupstream stopped", "upstream stopped"},
		{"bearer", init, "", "upstream stopped Bearer " + strings.Repeat("a", 40), "upstream stopped"},
		{"structured log", init, "", "AGY_ERROR: upstream failure " + secret + strings.Repeat("\nshutdown", 6), "AGY_ERROR:"},
		{"missing log", init, "", "", errAgyMissingResult.Error()},
		{"success", success, "AGY_ERROR: ignored", "upstream stopped", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := replayAgyStream(t, tc.stream, tc.stderr, tc.log)
			if tc.want == "" {
				if err != nil || res.Answer != "ok" {
					t.Fatalf("successful result changed: answer %q, err %v", res.Answer, err)
				}
				return
			}
			if !errors.Is(err, errAgyMissingResult) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing-result diagnostics incorrect (case %s)", tc.name)
			}
			for _, forbidden := range []string{secret, "earlier", strings.Repeat("s", 20), strings.Repeat("a", 40)} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("missing-result diagnostics unredacted or unbounded (case %s)", tc.name)
				}
			}
		})
	}
}

func replayAgyStream(t *testing.T, stream, stderr, log string) (wire.Result, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HARNESS_AGENT_BIN_DIR", dir)
	script := "#!/bin/sh\nprintf '%s\\n' '" + stream + "'\nprintf '%s\\n' '" + stderr + "' >&2\n"
	if err := os.WriteFile(filepath.Join(dir, "gemini"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agy.log")
	if log != "" {
		if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return runAgy(context.Background(), agyRequest{Prompt: "replay", DebugFile: path}, wire.Discard)
}
