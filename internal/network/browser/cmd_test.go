package browser

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestNetworkSubcommandsRejectUnknownFormat: an unknown --format is a usage error (2) before any work.
func TestNetworkSubcommandsRejectUnknownFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name string
		fn   func() int
	}{
		{"browser", func() int { return Cmd([]string{"--format", "yaml"}) }},
		{"browser --setup --dry-run", func() int {
			return Cmd([]string{"--setup", "--dry-run", "--format",
				"yaml"})
		}},
	}
	for _, c := range cases {
		code, _, stderr := hosttest.CaptureOutput(t, c.fn)
		if code != 2 {
			t.Errorf("network %s: code = %d, want 2; stderr=%q", c.name, code, stderr)
		}
	}
}

// TestNetworkBrowserSetupOnlyFlags: --browser and --dry-run belong to --setup; alone they are usage errors.
func TestNetworkBrowserSetupOnlyFlags(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"--browser", "chrome"}, {"--dry-run"}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
		if code != 2 || !strings.Contains(stderr, "belongs to --setup") {
			t.Errorf("%v: code=%d stderr=%q, want 2 + belongs to --setup", args, code, stderr)
		}
	}
}

// TestNetworkBrowserRejectsPositional: `--setup chrome` (forgotten --browser) must not silently set up the default.

func TestRenderText_DefaultMarkerAndProfileLine(t *testing.T) {
	r := Report{
		Default: "chrome",
		Browsers: []Browser{
			{
				Name: "chrome", DisplayName: "Google Chrome", Family: familyChromium,
				Installed: true, Default: true, Host: stateReady,
				Paths: controlPaths{Extension: stateReady, CDP: stateAvailable, WebDriver: stateUnsupported,
					GUI: stateAvailable},
				Profiles: []profile{{Name: "Default", Extension: stateReady}},
			},
			{
				Name: "safari", DisplayName: "Safari", Family: familySafari,
				Installed: true, Host: stateUnsupported,
				Paths: controlPaths{Extension: stateUnsupported, CDP: stateUnsupported, WebDriver: stateDisabled,
					GUI: stateAvailable},
			},
		},
	}
	var buf bytes.Buffer
	renderText(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "default: chrome") {
		t.Fatalf("output missing default line:\n%s", out)
	}
	if !strings.Contains(out, "chrome") || !strings.Contains(out, "(default)") {
		t.Fatalf("output missing chrome's (default) marker:\n%s", out)
	}
	if strings.Contains(strings.SplitN(out, "\n", 3)[2], "(default)") {
		t.Fatalf("safari row wrongly marked (default):\n%s", out)
	}
	if !strings.Contains(out, "profile Default") {
		t.Fatalf("output missing the Default profile line:\n%s", out)
	}
}

func TestRenderText_NoDefaultShowsDash(t *testing.T) {
	r := Report{Default: "", Browsers: nil}
	var buf bytes.Buffer
	renderText(&buf, r)
	if !strings.Contains(buf.String(), "default: -") {
		t.Fatalf("output = %q, want a dash when no default resolved", buf.String())
	}
}

type failingRenderWriter struct {
	writes, failAt int
	first, later   error
}

func (w *failingRenderWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.first
	}
	if w.writes > w.failAt {
		return 0, w.later
	}
	return len(p), nil
}

func TestRenderTextReturnsFirstWriteError(t *testing.T) {
	r := Report{Default: "chrome", Browsers: []Browser{
		{Name: "chrome", Profiles: []profile{{Name: "Default"}, {Name: "Other"}}},
		{Name: "firefox"},
	}}
	for _, failAt := range []int{1, 2, 3, 4, 5} {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			first := errors.New("first write failed")
			w := &failingRenderWriter{failAt: failAt, first: first, later: errors.New("later write failed")}
			if err := renderText(w, r); !errors.Is(err, first) {
				t.Fatalf("write %d: error=%v, want first writer error", failAt, err)
			}
			if w.writes != failAt {
				t.Fatalf("continued after failure at write %d: %d writes", failAt, w.writes)
			}
		})
	}
}
