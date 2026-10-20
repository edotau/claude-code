package generate

import "testing"

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
			got := insertTopLevelTOML(in, section)

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
			if !(idxString < idxInserted && idxInserted < idxRealTable) {
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
	got := insertTopLevelTOML(in, section)

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
	if !(idxArray < idxInserted && idxInserted < idxReal) {
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
