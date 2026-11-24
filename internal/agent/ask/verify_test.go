package ask

import (
	"context"
	"strings"
	"testing"
)

func TestRunVerifyRecordsExit(t *testing.T) {
	ok := runVerify(context.Background(), t.TempDir(), "go vet ./nonexistentpkg")
	if ok.Exit == 0 || ok.Tail == "" || ok.Command != "go vet ./nonexistentpkg" {
		t.Fatalf("failed step must record a non-zero exit and output: %+v", ok)
	}
	if bad := runVerify(context.Background(), "", "rm -rf x"); bad.Exit != -1 {
		t.Fatalf("a disallowed shape must not run: %+v", bad)
	}
}

func TestLineBriefOwn(t *testing.T) {
	got, err := lineBrief(batchLine{Prompt: "do it", Own: []string{"a/b.go"}}, "claude", false)
	if err != nil || !strings.Contains(got, `"a/b.go"`) {
		t.Fatalf("brief = %q, %v", got, err)
	}
	if plain, _ := lineBrief(batchLine{Prompt: "do it"}, "claude", false); plain != "do it" {
		t.Fatalf("no contract keys must leave the prompt: %q", plain)
	}
}
