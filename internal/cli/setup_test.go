package cli

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCmdSetupNukeRejectsPartialConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(home, "confirmation")
	if err := os.WriteFile(input, []byte(dir+" extra-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })

	code, out, _ := captureOutput(t, func() int {
		return cmdSetupNuke([]string{"--dir", dir, "--apply"})
	})
	if code == 0 {
		t.Error("partially scanned confirmation exit = 0, want refusal")
	}
	if strings.Contains(out, "nuke plan for") {
		t.Errorf("invalid confirmation reached Nuke: %q", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("target must survive invalid confirmation: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(home, "claude-nuke-*.tgz"))
	if err != nil || len(archives) != 0 {
		t.Errorf("invalid confirmation created archives: %v, %v", archives, err)
	}
}
