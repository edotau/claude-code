package browser

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderText_DefaultMarkerAndProfileLine(t *testing.T) {
	r := Report{
		Default: "chrome",
		Browsers: []Browser{
			{
				Name: "chrome", DisplayName: "Google Chrome", Family: familyChromium,
				Installed: true, Default: true, Host: stateReady,
				Paths:    Paths{Extension: stateReady, CDP: stateAvailable, WebDriver: stateUnsupported, GUI: stateAvailable},
				Profiles: []Profile{{Name: "Default", Extension: stateReady}},
			},
			{
				Name: "safari", DisplayName: "Safari", Family: familySafari,
				Installed: true, Host: stateUnsupported,
				Paths: Paths{Extension: stateUnsupported, CDP: stateUnsupported, WebDriver: stateDisabled, GUI: stateAvailable},
			},
		},
	}
	var buf bytes.Buffer
	RenderText(&buf, r)
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
	RenderText(&buf, r)
	if !strings.Contains(buf.String(), "default: -") {
		t.Fatalf("output = %q, want a dash when no default resolved", buf.String())
	}
}
