package transcript

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// Every shape `ask` prints is found, raw or escaped; prose outside a tool_result and repeats are ignored.
func TestDispatchIDs(t *testing.T) {
	const (
		exitID  = "20261030T101500.123-codex-aaaa"
		jobID   = "20261030T101501.456-claude-bbbb"
		bgID    = "20261030T101502.789-gemini-cccc"
		envID   = "20261030T101503.001-copilot-dddd"
		rawID   = "20261030T101504.002-codex-eeee"
		proseID = "20261030T101505.003-codex-ffff"
	)
	result := func(content string) string {
		return `{"type":"user","message":{"content":[{"type":"tool_result","content":` + content + `}]}}`
	}
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"job ` + proseID + ` started"}]}}`,
		result(`"ask: dispatch ` + exitID + ` (claude-code agents ask --retry <id> replays it)"`),
		result(`"job ` + jobID + ` started\n"`),
		result(`"{\"id\":\"` + bgID + `\",\"status\":\"running\"}"`),
		result(`[{"type":"text","text":"{\"agent\":\"codex\",\"dispatch_id\":\"` + envID + `\"}"}]`),
		result(`[{"type":"text","text":"{\\\"id\\\": \\\"` + rawID + `\\\"}"}]`),
		result(`"{\"id\":\"toolu_01\",\"dispatch_id\":\"` + envID + `\"}"`),
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{exitID, jobID, bgID, envID, rawID}
	if got := DispatchIDs(path); !slices.Equal(got, want) {
		t.Fatalf("DispatchIDs = %v, want %v", got, want)
	}
	if got := DispatchIDs(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing file: got %v, want nil", got)
	}
}

func TestDispatchIDsRawQuotes(t *testing.T) {
	const id = "20261030T101500.123-codex-aaaa"
	path := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"tool_result","id":"` + id + `"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DispatchIDs(path); !slices.Equal(got, []string{id}) {
		t.Fatalf("DispatchIDs = %v, want [%s]", got, id)
	}
}

func joinLines(lines []string) string {
	var s string
	var sSb59 strings.Builder
	for _, l := range lines {
		sSb59.WriteString(l + "\n")
	}
	s += sSb59.String()
	return s
}

// (5) transcript.DispatchIDs greps every "ask: dispatch <id>" exit line out of a relay transcript, deduped, in
// the order they first appear.
func TestDispatchIDsExtraction(t *testing.T) {
	path := filepath.Join("testdata", "dispatch_relay.jsonl")
	ids := DispatchIDs(path)
	want := []string{"20261017T231131.948-codex", "20261018T095401.709-gemini", "20261018T150000.000-codex"}
	if len(ids) != len(want) {
		t.Fatalf("DispatchIDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}
