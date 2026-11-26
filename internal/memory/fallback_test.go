package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

const goodBatch = `{"ops":[{"op":"add","target":"progress","section":"Known Issues","text":"placed by the fallback"}]}`

func fakeCodeMode(t *testing.T, answer string) *int {
	t.Helper()
	calls := 0
	old := CodeModeRun
	CodeModeRun = func(_ context.Context, _, _, _, _ string) (string, string, error) {
		calls++
		return answer, "fake-pi", nil
	}
	t.Cleanup(func() { CodeModeRun = old })
	return &calls
}

func TestTypedWriteFallsBackToCodeModeOnce(t *testing.T) {
	upgradeFixture(t)
	calls := fakeCodeMode(t, goodBatch)
	var fb CodeFallback
	if ans, who, err := fb.Run(context.Background(), "p", t.TempDir(), BatchSchema()); err != nil || who != "fake-pi" || ans != goodBatch {
		t.Fatalf("first run = %q, %q, %v", ans, who, err)
	}
	if _, _, err := fb.Run(context.Background(), "p", t.TempDir(), BatchSchema()); err == nil || *calls != 1 {
		t.Fatalf("second run err = %v after %d dispatches, want a refusal after 1", err, *calls)
	}
	bad := fakeCodeMode(t, `{"ops":[{"op":"nope"}]}`)
	if _, _, err := new(CodeFallback).Run(context.Background(), "p", t.TempDir(), BatchSchema()); err == nil || *bad != 1 {
		t.Fatalf("an invalid answer must be refused (err %v)", err)
	}
}

// standInStoreCopy swaps the git clone for a temp dir, for tests whose store is not a repo.
func standInStoreCopy(t *testing.T) {
	t.Helper()
	old := StoreCopy
	StoreCopy = func(context.Context) (string, func(), error) { return t.TempDir(), func() {}, nil }
	t.Cleanup(func() { StoreCopy = old })
}

func TestRefusedTellsCommitErrorsFromRefusals(t *testing.T) {
	after := &AfterCommitError{errors.New("git commit failed")}
	for err, want := range map[error]bool{
		errors.New("1 of 1 ops failed"):      true,
		after:                                false,
		fmt.Errorf("dream batch: %w", after): false,
		withFailedOps(after, []OpResult{{Outcome: DreamFailed}}): false,
		&ConflictError{Op: 0, Ref: "r"}:                          true,
	} {
		if got := refused(err); got != want {
			t.Errorf("refused(%v) = %v, want %v", err, got, want)
		}
	}
}
