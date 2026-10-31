package agent_test

import (
	"github.com/edotau/claude-code/internal/agent"
	"strings"
	"testing"
)

// agent.MergeCodexTrust must not mistake a "["-led line inside an open array for a table header: an
// existing trusted_hash AFTER the array, still inside the same table, is replaced in place —
// not left stale with a duplicate appended at EOF because the array wrongly reset the active table.
func TestMergeCodexTrustBracketInArrayNotHeader(t *testing.T) {
	raw := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"old\"\n"
	entries := map[string]string{"K": "abc123"}
	got := agent.MergeCodexTrust(raw, entries)
	want := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"abc123\"\n"
	if got != want {
		t.Fatalf("mergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
	if strings.Count(got, "trusted_hash") != 1 {
		t.Errorf("want exactly one trusted_hash line, got:\n%q", got)
	}
}

// A real table header following a "["-led array element must still pick up its own hash, proving
// the depth tracking resets once the array closes rather than staying stuck open.
func TestMergeCodexTrustHeaderAfterArrayStillMatches(t *testing.T) {
	raw := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\n"
	entries := map[string]string{"K2": "def456"}
	got := agent.MergeCodexTrust(raw, entries)
	want := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\ntrusted_hash = \"def456\"\n"
	if got != want {
		t.Fatalf("mergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
}

func TestMergeCodexTrustPreservesUnownedText(t *testing.T) {
	raw := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = 'old' # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
[foreign]
trusted_hash = "unowned"`
	want := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = "new" # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
trusted_hash = "added"
[foreign]
trusted_hash = "unowned"

[hooks.state."a"]
trusted_hash = "first"

[hooks.state."z"]
trusted_hash = "last"
`
	entries := map[string]string{"active": "new", "missing": "added", "z": "last", "a": "first"}
	got := agent.MergeCodexTrust(raw, entries)
	if got != want {
		t.Fatalf("mergeCodexTrust:\n%s\nwant:\n%s", got, want)
	}
	if again := agent.MergeCodexTrust(got, entries); again != got {
		t.Fatalf("merge is not idempotent:\n%s", again)
	}
	if len(entries) != 4 {
		t.Fatal("merge mutated input entries")
	}
}

// TestInsertTopLevelTOMLSkipsMultilineStrings: a "["-led line inside a TOML multiline string
// (triple-quoted) must not be mistaken for the first top-level table header.
func TestInsertTopLevelTOMLSkipsMultilineStrings(t *testing.T) {
	cases := []struct {
		name  string
		quote string
	}{
		{"doubleQuoted", `"""`},
		{"singleQuoted", `'''`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := "note = " + c.quote + "\nsome text\n[looks like a table]\nmore text\n" + c.quote + "\n\n[real.table]\nkey = 1\n"
			section := "[inserted]\nx = 1\n"
			got := agent.InsertTopLevelTOML(in, section)

			wantString := "note = " + c.quote + "\nsome text\n[looks like a table]\nmore text\n" + c.quote + "\n"
			idxString := indexOf(got, wantString)
			idxInserted := indexOf(got, "[inserted]")
			idxRealTable := indexOf(got, "[real.table]")

			if idxString == -1 {
				t.Fatalf("multiline string content not byte-for-byte unchanged, got:\n%s", got)
			}
			if idxInserted == -1 || idxRealTable == -1 {
				t.Fatalf("missing expected sections, got:\n%s", got)
			}
			if idxString >= idxInserted || idxInserted >= idxRealTable {
				t.Errorf("want order: multiline string, then [inserted], then [real.table]; got:\n%s", got)
			}
		})
	}
}

// TestInsertTopLevelTOMLSkipsNestedArrays: a "["-led line inside an open multiline array (nested
// array element, not a multiline string) must not be mistaken for the first top-level table header.
func TestInsertTopLevelTOMLSkipsNestedArrays(t *testing.T) {
	in := "matrix = [\n  [1, 2],\n  [3, 4],\n]\n\n[real]\nk = 1\n"
	section := "[inserted]\nx = 1\n"
	got := agent.InsertTopLevelTOML(in, section)

	wantArray := "matrix = [\n  [1, 2],\n  [3, 4],\n]\n"
	idxArray := indexOf(got, wantArray)
	idxInserted := indexOf(got, "[inserted]")
	idxReal := indexOf(got, "[real]")

	if idxArray == -1 {
		t.Fatalf("array bytes not byte-for-byte unchanged, got:\n%s", got)
	}
	if idxInserted == -1 || idxReal == -1 {
		t.Fatalf("missing expected sections, got:\n%s", got)
	}
	if idxArray >= idxInserted || idxInserted >= idxReal {
		t.Errorf("want order: array, then [inserted], then [real]; got:\n%s", got)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
