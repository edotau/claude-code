package hosttest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

func TestCaptureOutputRestoresStreamsAfterGoexit(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*testing.T)
	}{
		{"fatal", func(it *testing.T) { it.Fatal("callback stopped") }},
		{"skip", func(it *testing.T) { it.SkipNow() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldOut, oldErr := os.Stdout, os.Stderr
			t.Cleanup(func() {
				if os.Stdout != oldOut {
					if err := os.Stdout.Close(); err != nil {
						t.Error(err)
					}
				}
				if os.Stderr != oldErr {
					if err := os.Stderr.Close(); err != nil {
						t.Error(err)
					}
				}
				os.Stdout, os.Stderr = oldOut, oldErr
			})
			testing.RunTests(func(_, _ string) (bool, error) { return true, nil },
				[]testing.InternalTest{{Name: tc.name, F: func(it *testing.T) {
					CaptureOutput(it, func() int {
						tc.stop(it)
						return 0
					})
				}}})
			if os.Stdout != oldOut {
				t.Error("CaptureOutput did not restore os.Stdout after " + tc.name)
			}
			if os.Stderr != oldErr {
				t.Error("CaptureOutput did not restore os.Stderr after " + tc.name)
			}
		})
	}
}

func TestOneDocRejectsStrayClosingDelimiter(t *testing.T) {
	for _, tc := range []struct {
		input string
		valid bool
	}{
		{"{} \n", true},
		{"{}]", false},
		{"{}}", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			passed := testing.RunTests(func(_, _ string) (bool, error) { return true, nil },
				[]testing.InternalTest{{Name: "one-doc", F: func(it *testing.T) {
					OneDoc(it, tc.input)
				}}})
			if passed != tc.valid {
				t.Fatalf("OneDoc(%q) passed = %v, want %v", tc.input, passed, tc.valid)
			}
		})
	}
}

func TestHostIsHermetic(t *testing.T) {
	for _, vendor := range []string{"claude", "codex", "copilot", "opencode", "agy", "gemini", "tmux", "tailscale", "launchctl"} {
		if p, err := exec.LookPath(vendor); err == nil {
			t.Errorf("%s visible at %s", vendor, p)
		}
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Errorf("sh missing from the allowlisted PATH: %v", err)
	}
	cfg, home := os.Getenv("CLAUDE_CONFIG_DIR"), os.Getenv("HOME")
	if !strings.HasPrefix(cfg, home) || !strings.Contains(home, "hosttest-") {
		t.Errorf("CLAUDE_CONFIG_DIR %q / HOME %q not in the temp dir", cfg, home)
	}
	if os.Getenv("HARNESS_VAULT_KEY") == "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		t.Error("vault key unset or WSL marker leaked")
	}
}
