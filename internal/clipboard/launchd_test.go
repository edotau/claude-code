package clipboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plist lands under a temp HOME, launchctl (mocked) boots out then bootstraps once, and a rerun is idempotent.
func TestLaunchAgentInstall(t *testing.T) {
	home := t.TempDir()
	bindir := t.TempDir()
	log := filepath.Join(home, "launchctl.log")
	if err := os.WriteFile(filepath.Join(bindir, "launchctl"), []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+":/usr/bin:/bin")
	bin := "/opt/cc/claude-code"

	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(launchPlistPath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{bin, "<string>--serve</string>", launchSocket(home), "inetdCompatibility", launchLabel} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Dir(launchSocket(home))); err != nil {
		t.Errorf("run dir not created: %v", err)
	}
	calls, _ := os.ReadFile(log)
	if got := string(calls); got != "bootout gui/501/"+launchLabel+"\nbootstrap gui/501 "+launchPlistPath(home)+"\n" {
		t.Fatalf("launchctl calls = %q", got)
	}

	out.Reset()
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(log); !bytes.Equal(again, calls) {
		t.Fatalf("unchanged plist reloaded: %q", again)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("rerun output = %q", out.String())
	}
}

func TestLaunchAgentDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, "/x/claude-code", 501, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launchPlistPath(home)); err == nil {
		t.Fatal("dry run wrote the plist")
	}
}
