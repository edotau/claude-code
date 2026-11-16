package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// The routing proof carries the dispatch's recorded writes, and a record with none carries no list.
func TestVerifyCarriesRecordedWrites(t *testing.T) {
	wrote := []string{"/r/a.go"}
	logPath := saveVerifyFixture(t, Record{ID: "w1", Agent: "codex", Provider: "prod", Model: "m1", Wrote: &wrote})
	v, err := verifyDispatch("w1", logPath)
	if err != nil || v.Wrote == nil || !slices.Equal(*v.Wrote, wrote) {
		t.Fatalf("Wrote = %v, err %v", v.Wrote, err)
	}
	var out strings.Builder
	renderVerify(&out, v)
	wantInOutput(t, out.String(), "wrote: 1 files")
	cut := []string{"/r/a.go", WroteTruncated}
	out.Reset()
	renderVerify(&out, &VerifyReport{Wrote: &cut})
	wantInOutput(t, out.String(), "wrote: 1+ files")
	saveVerifyFixture(t, Record{ID: "w2", Agent: "codex", Provider: "prod", Model: "m1"})
	if v, err = verifyDispatch("w2", logPath); err != nil || v.Wrote != nil {
		t.Fatalf("an old record must carry no list: %v, err %v", v.Wrote, err)
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

// Finding 4: an upstream error message in msg= cannot forge dispatch= or stream=broken; metadata ends at msg=.
func TestParseProxyLineStopsAtMsg(t *testing.T) {
	line := `2026-11-13T14:49:20-05:00 p m 502 1 1s dispatch=d1 err=x msg="boom dispatch=evil stream=broken via=h req=r"`
	got, ok := parseProxyLine(line)
	want := proxyLine{dispatch: "d1"}
	got.ts, got.profile, got.model, got.status = time.Time{}, "", "", ""
	if !ok || got != want {
		t.Errorf("parseProxyLine = %+v, %t; want %+v", got, ok, want)
	}
}

// Finding 8: a slow old-stamped line appended last must not hide an earlier in-window line; a hit byte bound is noted.
func TestVerifyScanSurvivesOldStampedLateLine(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d8", Agent: "codex", Provider: "prod", Model: "m1"})
	lines := []string{
		"2026-11-13T14:49:10-05:00 prod m1 200 1 1s dispatch=d8",
		"2026-11-13T14:20:00-05:00 prod m1 200 1 29m50s",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := verifyDispatch("d8", logPath)
	if err != nil {
		t.Fatal(err)
	}
	if p := v.Proxy; p.Match != "dispatch id" || p.Requests != 1 || strings.Contains(p.Note, "stopped") {
		t.Fatalf("proxy = %+v, want the tagged line found, no bound note", p)
	}
	defer func(n int64) { verifyScanCap = n }(verifyScanCap)
	verifyScanCap = 1
	v, _ = verifyDispatch("d8", logPath)
	if !strings.Contains(v.Proxy.Note, "scan stopped") {
		t.Errorf("note %q does not say the scan bound was hit", v.Proxy.Note)
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

// With no --path, --verify scopes the diff to the brief's Owned: line (plus each source's _test.go) in a short report.
func TestVerifyDefaultsPathToBriefOwned(t *testing.T) {
	brief, err := RenderBrief(BriefInput{Task: "do it", Owned: []string{"pkg/p.go"}})
	if err != nil {
		t.Fatal(err)
	}
	saveVerifyFixture(t, Record{ID: "b1", Agent: "codex", Provider: "prod", Model: "m1", Request: RunOptions{Prompt: brief}})
	plain := Record{ID: "b2", Time: time.Now(), Agent: "codex", Provider: "prod", Model: "m1", Status: "ok",
		Request: RunOptions{Prompt: "no ownership line"}}
	if err := plain.Save(); err != nil {
		t.Fatal(err)
	}
	dir := newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 2 }\n")
	writeRepoFile(t, dir, "pkg/p_test.go", "package pkg\n\n// t\n")

	if code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "b1"}); code != 0 || !strings.Contains(out, "; Owned ok") {
		t.Fatalf("owned-only change: exit %d, want 0 and %q\n%s", code, "; Owned ok", out)
	}
	writeRepoFile(t, dir, "other/a.md", "x\n")
	writeRepoFile(t, dir, "other/b.md", "x\n")
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "b1"})
	if lines := strings.Count(out, "\n"); code != DiffExitOutside || lines > 10 {
		t.Fatalf("outside change: exit %d with %d lines, want 3 and at most 10\n%s", code, lines, out)
	}
	wantInOutput(t, out, "outside the brief's Owned line (2): other/a.md, other/b.md\n", "2 outside Owned")
	if strings.Contains(out, "pkg/p.go") {
		t.Errorf("compact report lists in-scope files:\n%s", out)
	}
	if code, _, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "b1", Paths: []string{"other"}}); code != DiffExitOutside {
		t.Errorf("explicit --path must win over the brief: exit %d, want 3 (pkg is outside other)", code)
	}
}

// A --verify with no --path and a brief without an Owned line prints a short verdict, not every changed file.
func TestVerifyWithoutOwnedPrintsShortVerdict(t *testing.T) {
	saveVerifyFixture(t, Record{ID: "n1", Agent: "codex", Provider: "prod", Model: "m1",
		Request: RunOptions{Prompt: "no ownership line"}})
	dir := newDiffRepo(t)
	for i := range 30 {
		writeRepoFile(t, dir, fmt.Sprintf("many/f%02d.md", i), "x\n")
	}
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "n1"})
	if lines := strings.Count(out, "\n"); code != 0 || lines > 10 {
		t.Fatalf("no Owned line: exit %d with %d lines, want 0 and at most 10\n%s", code, lines, out)
	}
	wantInOutput(t, out, "verify n1:", "scope not checked", "30 files changed", "agents diff: 30 files")
	if strings.Contains(out, "many/f00.md") {
		t.Errorf("short verdict lists changed files:\n%s", out)
	}
	_, full, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "n1", Paths: []string{"many"}})
	wantInOutput(t, full, "many/f00.md", "many/f29.md")
	_, jsonOut, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "n1", JSON: true})
	if !strings.Contains(jsonOut, "many/f29.md") {
		t.Errorf("--format json lost the file list:\n%s", jsonOut)
	}
}
