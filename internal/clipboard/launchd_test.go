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
	loaded := filepath.Join(home, "loaded") // present = launchd has the agent; print fails without it
	fake := "#!/bin/sh\nif [ \"$1\" = print ]; then [ -e " + loaded + " ]; exit $?; fi\necho \"$@\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bindir, "launchctl"), []byte(fake), 0o755); err != nil {
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

	// Current plist, agent not loaded (booted out): rerun bootstraps it instead of reporting current.
	out.Reset()
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := os.ReadFile(log)
	if want := string(calls) + "bootstrap gui/501 " + launchPlistPath(home) + "\n"; string(reloaded) != want {
		t.Fatalf("unloaded agent: launchctl calls = %q, want a bootstrap", reloaded)
	}

	if err := os.WriteFile(loaded, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(log); !bytes.Equal(again, reloaded) {
		t.Fatalf("unchanged loaded plist reloaded: %q", again)
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
