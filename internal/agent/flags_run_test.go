package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeCLI installs a shell stand-in for name that records its argv (one per line) to the returned file, then runs body.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(t.TempDir(), name+".argv")
	script := "#!/bin/sh\n" + body + "\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argv + "'.tmp\n"
	if err := os.WriteFile(filepath.Join(paths.BinDir(), name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return argv
}

func readArgv(t *testing.T, argv string) []string {
	t.Helper()
	b, err := os.ReadFile(argv + ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// followedBy reports whether args holds flag immediately followed by val.
func followedBy(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

// codex gets -c instructions / developer_instructions and a strict --output-schema file that is gone after the run.
func TestCodexRunFlagsAndSchemaFileCleanup(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	registry(t, srv)
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(`{"providers":{"openai":{"toai":{"protocol":"responses","base_url":%q,"auth":{"type":"none"},"models":{"opus":"gpt-x"}}}}}`, srv.URL+"/v1"))
	capture := filepath.Join(t.TempDir(), "schema.cap")
	// help prints no --json: codexHasJSON is cached per process and must not flip for other tests
	body := `cat >/dev/null
if [ "$1" = exec ] && [ "$2" = --help ]; then exit 0; fi
prev=
for a in "$@"; do
  if [ "$prev" = --output-schema ]; then cp "$a" '` + capture + `'; printf '%s\n' "$a" > '` + capture + `.path'; ls -l "$a" | cut -c1-10 > '` + capture + `.mode'; fi
  prev=$a
done
echo '{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"ok"}}'`
	argv := fakeCLI(t, "codex", body)
	_, err := newCLIEngine("codex").Run(context.Background(), RunOptions{Prompt: "task", Provider: "toai", Tools: true,
		SystemPrompt: `be "terse"`, AppendSystemPrompt: "APP", OutputSchema: `{"type":"object","properties":{"a":{"type":"string"}}}`}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	args := readArgv(t, argv)
	if !followedBy(args, "-c", `instructions="be \"terse\""`) || !followedBy(args, "-c", `developer_instructions="APP"`) {
		t.Errorf("argv %q lacks the -c instructions overrides", args)
	}
	if args[0] != "exec" || !slices.Contains(args, "--output-schema") {
		t.Errorf("argv %q: want exec first and --output-schema", args)
	}
	schema, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"additionalProperties":false`) || !strings.Contains(string(schema), `"required":["a"]`) {
		t.Errorf("schema handed to codex = %s", schema)
	}
	if mode, _ := os.ReadFile(capture + ".mode"); strings.TrimSpace(string(mode)) != "-rw-------" {
		t.Errorf("schema file mode = %q, want -rw-------", mode)
	}
	path, _ := os.ReadFile(capture + ".path")
	if _, err := os.Stat(strings.TrimSpace(string(path))); !os.IsNotExist(err) {
		t.Errorf("schema temp file %s survived the run (err=%v)", path, err)
	}
}
