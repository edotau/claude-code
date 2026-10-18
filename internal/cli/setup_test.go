package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCmdSetupNoArgs(t *testing.T) {
	if got := cmdSetup(nil); got != 2 {
		t.Fatalf("no-args exit = %d, want 2", got)
	}
}

func TestCmdSetupUnknownMode(t *testing.T) {
	if got := cmdSetup([]string{"bogus"}); got != 1 {
		t.Fatalf("unknown mode exit = %d, want 1", got)
	}
}

func TestCmdSetupAdoptRequiresFrom(t *testing.T) {
	if got := cmdSetupAdopt(nil); got != 2 {
		t.Fatalf("missing --from exit = %d, want 2", got)
	}
}

func TestCmdSetupAdoptRefusesExistingGit(t *testing.T) {
	into := t.TempDir()
	if err := os.MkdirAll(filepath.Join(into, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	from := t.TempDir()
	if got := cmdSetupAdopt([]string{"--from", from, "--into", into}); got != 2 {
		t.Fatalf("existing .git exit = %d, want 2", got)
	}
}

func TestCmdSetupNukeApplyWithoutConfirmRefused(t *testing.T) {
	dir := t.TempDir()
	// --apply prompts for confirmation on stdin; an empty/closed stdin never matches dir.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	oldStdin := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = oldStdin }()

	if got := cmdSetupNuke([]string{"--dir", dir, "--apply"}); got != 2 {
		t.Fatalf("apply without a matching confirm exit = %d, want 2", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dir must survive a refused nuke")
	}
}
