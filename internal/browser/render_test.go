package browser

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestReport_JSONRoundTrip(t *testing.T) {
	want := Report{
		Default:     "chrome",
		GeneratedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		Browsers: []Browser{
			{
				Name: "chrome", DisplayName: "Google Chrome", Family: familyChromium,
				Installed: true, BundleID: "com.google.Chrome", Default: true, Host: stateReady,
				Profiles: []Profile{{Name: "Default", Extension: stateReady}},
				Paths:    Paths{Extension: stateReady, CDP: stateAvailable, WebDriver: stateUnsupported, GUI: stateAvailable},
			},
			{
				Name: "edge", DisplayName: "Microsoft Edge", Family: familyChromium,
				Installed: false, Host: stateNotInstalled,
				Paths: Paths{Extension: stateNotInstalled, CDP: stateNotInstalled, WebDriver: stateNotInstalled, GUI: stateNotInstalled},
			},
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Fatalf("GeneratedAt = %v, want %v", got.GeneratedAt, want.GeneratedAt)
	}
	if got.Default != want.Default {
		t.Fatalf("Default = %q, want %q", got.Default, want.Default)
	}
	if len(got.Browsers) != len(want.Browsers) {
		t.Fatalf("Browsers len = %d, want %d", len(got.Browsers), len(want.Browsers))
	}
	if !reflect.DeepEqual(got.Browsers[0], want.Browsers[0]) {
		t.Fatalf("Browsers[0] = %+v, want %+v", got.Browsers[0], want.Browsers[0])
	}
	if got.Browsers[1].Profiles != nil {
		t.Fatalf("edge Profiles = %+v, want omitted/nil (omitempty) round-tripping to nil", got.Browsers[1].Profiles)
	}
}

func TestSelectPath_SignedInSessionPrefersExtension(t *testing.T) {
	b := Browser{Family: familyChromium, Paths: Paths{Extension: stateReady}}
	if got := SelectPath(b, NeedSignedInSession); got != pathExtension {
		t.Fatalf("SelectPath = %q, want extension", got)
	}
}

func TestSelectPath_SignedInSessionFallsBackToGUI(t *testing.T) {
	b := Browser{Family: familyChromium, Paths: Paths{Extension: stateNoExtension}}
	if got := SelectPath(b, NeedSignedInSession); got != pathGUI {
		t.Fatalf("SelectPath = %q, want gui", got)
	}
}

func TestSelectPath_AutomationPrefersCDPForChromium(t *testing.T) {
	b := Browser{Family: familyChromium, Paths: Paths{CDP: stateAvailable}}
	if got := SelectPath(b, NeedAutomation); got != pathCDP {
		t.Fatalf("SelectPath = %q, want cdp", got)
	}
}

func TestSelectPath_AutomationFallsBackToWebDriverThenGUI(t *testing.T) {
	ff := Browser{Family: familyFirefox, Paths: Paths{WebDriver: stateAvailable}}
	if got := SelectPath(ff, NeedAutomation); got != pathWebDriver {
		t.Fatalf("SelectPath(firefox) = %q, want webdriver", got)
	}
	safari := Browser{Family: familySafari, Paths: Paths{WebDriver: stateDisabled}}
	if got := SelectPath(safari, NeedAutomation); got != pathGUI {
		t.Fatalf("SelectPath(safari, webdriver disabled) = %q, want gui", got)
	}
}
