package transcript

import (
	"encoding/json"
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
	want := []string{exitID, jobID, bgID, envID, rawID}
	if got := dispatchIDs(strings.Join(lines, "\n")); !slices.Equal(got, want) {
		t.Fatalf("dispatchIDs = %v, want %v", got, want)
	}
}

// dispatchIDs folds every line of content through AppendDispatchIDs.
func dispatchIDs(content string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		ids = AppendDispatchIDs(ids, seen, []byte(line))
	}
	return ids
}

func TestDispatchIDsRawQuotes(t *testing.T) {
	const id = "20261030T101500.123-codex-aaaa"
	if got := dispatchIDs(`{"type":"tool_result","id":"` + id + `"}`); !slices.Equal(got, []string{id}) {
		t.Fatalf("dispatchIDs = %v, want [%s]", got, id)
	}
}

// (5) AppendDispatchIDs greps every "ask: dispatch <id>" exit line out of a relay transcript, deduped, in
// the order they first appear.
func TestDispatchIDsExtraction(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dispatch_relay.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ids := dispatchIDs(string(raw))
	want := []string{"20261017T231131.948-codex", "20261018T095401.709-gemini", "20261018T150000.000-codex"}
	if len(ids) != len(want) {
		t.Fatalf("dispatchIDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestEachLineOverflowAndUnterminated(t *testing.T) {
	giant := strings.Repeat("x", 3<<20) + "\n"
	in := "a\n" + giant + "b\nlast"
	var got []string
	EachLine(strings.NewReader(in), func(l []byte) { got = append(got, string(l)) })
	want := []string{"a\n", giant, "b\n", "last"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: len %d, want len %d", i, len(got[i]), len(want[i]))
		}
	}
}

func TestContentText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bare string", `"hello"`, "hello"},
		{"text and tool_use", `[{"type":"text","text":"a"},{"type":"tool_use","name":"X"},{"type":"text","text":"b"}]`, "ab"},
		{"empty array", `[]`, ""},
		{"invalid json", `{not json`, ""},
		{"empty input", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ContentText(json.RawMessage(c.in)); got != c.want {
				t.Errorf("ContentText(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
