package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// saveVerifyFixture stores a dispatch that ran 14:49:09..14:50:09 (-05:00) and a proxy log around that window.
func saveVerifyFixture(t *testing.T, rec Record) string {
	t.Helper()
	setConfigDir(t)
	rec.Time, rec.DurationMs, rec.Status = time.Date(2026, 11, 13, 19, 49, 9, 0, time.UTC), 60_000, "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	log := strings.Join([]string{
		"proxy: listening on 127.0.0.1:1",
		"2026-11-13T14:49:50Z prod m1 200 1 1s",
		"2026-11-13T14:48:00-05:00 prod m1 200 1 1s",
		"2026-11-13T14:49:10-05:00 prod m1 200 1 1s via=h1 req=r1",
		"2026-11-13T14:49:20-05:00 prod m1 200 1 1s via=h1 stream=broken",
		"2026-11-13T14:49:30-05:00 prod m1 429 1 1s",
		"2026-11-13T14:49:45-05:00 dev m1 200 1 1s",
		"2026-11-13T14:49:46-05:00 prod m2 200 1 1s",
		"2026-11-13T19:49:50Z prod m1 200 1 1s",
		"2026-11-13T14:51:00-05:00 prod m1 200 1 1s",
	}, "\n") + "\n"
	p := filepath.Join(paths.StateDir(), "proxy.log")
	writeRepoFile(t, filepath.Dir(p), filepath.Base(p), log)
	return p
}

func wantInOutput(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("output lacks %q:\n%s", w, text)
		}
	}
}

// The window keeps 4 of the 9 lines: the Z-form line counts, the same instant read as 14:49:50Z does not.
func TestVerifyCountsProxyLinesInTheWindow(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d1", Agent: "codex", Provider: "prod", Model: "m1", Route: "databricks:prod"})
	v, err := verifyDispatch("d1", logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Proxy
	got.Note = ""
	want := ProxyMatch{Match: "time window", Requests: 4, Remote: 2, Hosts: []string{"h1"}, Broken: 1, NonOK: map[string]int{"429": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proxy = %+v, want %+v", got, want)
	}
	var out strings.Builder
	renderVerify(&out, v)
	wantInOutput(t, out.String(), "ran on: codex, workspace prod (databricks:prod), model m1", "fallback: none",
		"no proxy log line carries dispatch=d1", "fell back to the time window", "may include other traffic", "via remote host: 2 (h1)",
		"stream=broken: 1", "non-200: 429 x1")
	if v.Mismatch || strings.Contains(out.String(), "MISMATCH") {
		t.Errorf("a run as requested must not mismatch: %v", v.Mismatches)
	}
}

// A dispatch= tag selects its own lines exactly, even from a concurrent dispatch on the same workspace and model.
func TestVerifyMatchesByDispatchTag(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d3", Agent: "codex", Provider: "prod", Model: "m1"})
	lines := []string{
		"2026-11-13T14:49:10-05:00 prod m1 200 1 1s via=h1 dispatch=d3",
		"2026-11-13T14:49:20-05:00 prod m1 200 1 1s dispatch=d3 stream=broken:reset",
		"2026-11-13T14:49:25-05:00 prod m1 429 1 1s dispatch=other",
		"2026-11-13T14:49:30-05:00 prod m1 200 1 1s",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := verifyDispatch("d3", logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Proxy
	got.Note = ""
	want := ProxyMatch{Match: "dispatch id", Requests: 2, Remote: 1, Hosts: []string{"h1"}, Broken: 1, NonOK: map[string]int{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proxy = %+v, want %+v", got, want)
	}
}

// With no tag for this dispatch the window fallback counts untagged lines only, never another dispatch's.
func TestVerifyFallbackSkipsOtherDispatchLines(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d4", Agent: "codex", Provider: "prod", Model: "m1"})
	lines := []string{
		"2026-11-13T14:49:25-05:00 prod m1 429 1 1s dispatch=other",
		"2026-11-13T14:49:30-05:00 prod m1 200 1 1s",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := verifyDispatch("d4", logPath)
	if err != nil {
		t.Fatal(err)
	}
	if p := v.Proxy; p.Match != "time window" || p.Requests != 1 || len(p.NonOK) != 0 {
		t.Errorf("proxy = %+v, want the one untagged line by time window", p)
	}
}

// parseProxyLine reads the dispatch token and both stream=broken forms.
func TestParseProxyLineTokens(t *testing.T) {
	for line, want := range map[string]proxyLine{
		"2026-11-13T14:49:20-05:00 p m 200 1 1s via=h dispatch=d1 stream=broken":   {via: "h", dispatch: "d1", broken: true},
		"2026-11-13T14:49:20-05:00 p m 200 1 1s dispatch=d1 stream=broken:timeout": {dispatch: "d1", broken: true},
		"2026-11-13T14:49:20-05:00 p m 200 1 1s stream=brokenish":                  {},
	} {
		got, ok := parseProxyLine(line)
		got.ts, got.profile, got.model, got.status = time.Time{}, "", "", ""
		if !ok || got != want {
			t.Errorf("parseProxyLine(%q) = %+v, %t; want %+v", line, got, ok, want)
		}
	}
}

func TestVerifyMismatchExitsOneAndUnknownIDExitsTwo(t *testing.T) {
	saveVerifyFixture(t, Record{ID: "d2", Agent: "codex", Provider: "prod", Model: "m1", Fallback: "claude",
		FallbackReason: "429", Request: RunOptions{Model: "other", Provider: "qa"}})
	dir := newDiffRepo(t)
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "d2"})
	if code != DiffExitGate {
		t.Errorf("exit %d, want 1", code)
	}
	wantInOutput(t, out, "MISMATCH fallback: claude -> codex (429)", "MISMATCH workspace:", "MISMATCH model:")
	code, jsonOut, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "d2", JSON: true})
	var rep DiffReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil || code != DiffExitGate || rep.Verify == nil || !rep.Verify.Mismatch {
		t.Errorf("json exit %d err %v verify %+v", code, err, rep.Verify)
	}
	code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Verify: "nope"})
	if code != DiffExitUsage || out != "" {
		t.Errorf("unknown id: exit %d stdout %q, want 2 and none", code, out)
	}
	wantInOutput(t, errOut, `--verify "nope"`)
}
