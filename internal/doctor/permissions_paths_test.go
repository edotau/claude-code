package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPathRowsCoverEveryCaseAndAgent(t *testing.T) {
	names := []string{"claude", "codex", "copilot", "gemini", "opencode"}
	rows, tally := permPathRows(names, t.TempDir())
	if len(rows) != len(pathCases()) {
		t.Fatalf("%d rows for %d cases", len(rows), len(pathCases()))
	}
	for _, r := range rows {
		for _, a := range names {
			c, ok := r.Cells[a]
			if !ok || !slices.Contains([]string{"ok", "denied", "n/a", verdictRuntime}, c.Verdict) {
				t.Errorf("%s %s %s: cell %+v", r.Path, r.Case, a, c)
			}
			if c.Verdict == "n/a" && c.Why == "" {
				t.Errorf("%s %s %s: an n/a cell names why", r.Path, r.Case, a)
			}
		}
	}
	if tally.total == 0 || tally.met > tally.total {
		t.Fatalf("tally %+v", tally)
	}
}

// A path that launches in a narrower or wider mode than the ask path for the same request is denied, scope named.
func TestPathCellNamesAScopeTheAskPathDoesNotGrant(t *testing.T) {
	for _, tc := range []struct{ got, ref []string }{{nil, []string{"--tools"}}, {[]string{"--tools"}, []string{"--read-only"}}} {
		pc := pathCase{path: "x", name: "y", derive: askRef(tc.got...), ref: askRef(tc.ref...)}
		c := pathCell(pc, "copilot", t.TempDir())
		if c.Verdict != "denied" || !strings.HasPrefix(c.Denied[0], "scope: ask runs ") {
			t.Fatalf("got %v want %v: %+v", tc.got, tc.ref, c)
		}
	}
}

// A path that builds its own policy is judged probe by probe: a read-only launch that allows an edit is a miss.
func TestPathCellDeniesAPolicyThatWidensTheAskPath(t *testing.T) {
	derive := func(name, wd string) pathLaunch {
		l := askRef("--read-only")(name, wd)
		l.judge = fixed("allow", "")
		return l
	}
	c := pathCell(pathCase{path: "live", name: "read-only", derive: derive, ref: askRef("--read-only")}, "gemini", t.TempDir())
	if c.Verdict != "denied" || !slices.ContainsFunc(c.Denied, func(d string) bool { return strings.HasPrefix(d, "edit an owned file: path allow") }) {
		t.Fatalf("an unlayered live policy must be denied: %+v", c)
	}
}

func TestPathCellKeepsNAAndRuntimeOutOfTheTally(t *testing.T) {
	for _, l := range []pathLaunch{{skip: "no such path"}, {why: "needs a live run"}} {
		c := pathCell(pathCase{derive: func(string, string) pathLaunch { return l }}, "codex", t.TempDir())
		if c.Verdict == "ok" || c.Verdict == "denied" || c.Why == "" {
			t.Fatalf("%+v: %+v", l, c)
		}
	}
}

func TestLiveLaunchHasNoPathWhereTheAgentHasNoLiveMode(t *testing.T) {
	for _, a := range []string{"codex", "copilot", "opencode"} {
		if l := liveLaunch(false)(a, t.TempDir()); l.skip == "" {
			t.Errorf("%s has no live mode: %+v", a, l)
		}
	}
	if l := liveLaunch(true)("claude", t.TempDir()); l.skip != "" || l.fail != "" || (l.judge == nil && l.why == "") {
		t.Fatalf("claude's live child is built offline and judged: %+v", l)
	}
}

func TestToolScopeDeniesAToolTheLaunchNeverGrants(t *testing.T) {
	j := toolScope("Read,Bash", fixed("allow", ""))
	if r := j("Edit", "a.go"); r.verdict != "deny" {
		t.Fatalf("Edit is not in --tools: %+v", r)
	}
	if r := j("Read", "a.go"); r.verdict != "allow" {
		t.Fatalf("Read is granted: %+v", r)
	}
	if r := toolScope("", fixed("allow", ""))("Read", "a.go"); r.verdict != "deny" {
		t.Fatalf("an empty --tools grants nothing: %+v", r)
	}
}

func TestRelayCeilingIsTheWidestCommandTheWrapperGives(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "vet: `claude-code codex ask \"<brief>\"`\nread: `claude-code codex ask --read-only`\n" +
		"rescue: `claude-code codex ask --tools [--background] \"<brief>\"`\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "codex.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := relayIn(dir, "codex"); l.mode != "tools" || strings.Contains(l.via, "<") {
		t.Fatalf("the rescue command is the ceiling: %+v", l)
	}
	if l := relayIn(dir, "gemini"); l.skip == "" {
		t.Fatalf("a config with no wrapper has no relay path: %+v", l)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "gemini.md"), []byte("no command here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := relayIn(dir, "gemini"); l.fail == "" || l.mode != "" {
		t.Fatalf("a wrapper with no ask command is a miss: %+v", l)
	}
}

func TestDoctorPermissionsListsTheLaunchPaths(t *testing.T) {
	var text, js bytes.Buffer
	doctorPermissions(&text, "claude", false)
	doctorPermissions(&js, "claude", true)
	for _, want := range []string{"launch paths:", "job ", "background ", "batch ", "live ", "relay "} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the text report lacks %q", want)
		}
	}
	if !strings.Contains(js.String(), `"Paths"`) || !strings.Contains(js.String(), `"path": "relay"`) {
		t.Fatalf("the JSON report lacks the Paths section")
	}
}

// Each path derives the scope of the request from its own builder, so the modes follow the request kind.
func TestPathLaunchesFollowTheRequestKind(t *testing.T) {
	wd := t.TempDir()
	off := false
	for _, tc := range []struct {
		name, want string
		launch     pathLaunch
	}{
		{"job tools", "tools", jobLaunch(false)("codex", wd)},
		{"job read-only", "read-only", jobLaunch(true)("codex", wd)},
		{"batch tools", "tools", batchLaunch(nil, nil)("codex", wd)},
		{"batch read-only", "read-only", batchLaunch([]string{"--read-only"}, nil)("codex", wd)},
		{"batch tools=false", "no flag", batchLaunch(nil, &off)("codex", wd)},
		{"background transfer", "tools", transferLaunch(true)("codex", wd)},
		{"background explore", "read-only", exploreLaunch(true)("copilot", wd)},
	} {
		if tc.launch.mode != tc.want {
			t.Errorf("%s: mode %q (%s), want %q", tc.name, tc.launch.mode, tc.launch.via, tc.want)
		}
	}
}
