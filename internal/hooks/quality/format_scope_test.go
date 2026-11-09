package quality

import "testing"

func TestMakeFormatRejectsFileAndWholeTreeArguments(t *testing.T) {
	_, mine, other := makeRepo(t, "format:\n\tgofmt -w $(FILE) .\n")
	if makeFormat(mine) {
		t.Fatal("a command naming both the target file and the whole tree must not run")
	}
	if formatted(t, mine) || formatted(t, other) {
		t.Fatal("rejecting the whole-tree command must leave both files untouched")
	}
	if !formatFile(mine) || !formatted(t, mine) || formatted(t, other) {
		t.Fatal("the fallback must format only the target file")
	}
}
