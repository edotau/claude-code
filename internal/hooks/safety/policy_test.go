package safety

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/settings"
)

func TestParsePolicyEditRows(t *testing.T) {
	p := parsePolicy(settings.PermissionLists{Deny: []string{"Edit(.env)", "Write(~/x)", "Edit", "Write", "Edit( )", "Edit()"}})
	if want := []string{".env", "~/x", "**/*", "**/*"}; !slices.Equal(p.editDeny, want) {
		t.Errorf("editDeny = %q, want %q", p.editDeny, want)
	}
}

func TestRelativeEditGlobAnchorsAtCwd(t *testing.T) {
	if got := foldedGlobs([]string{".env"}, "/w/app"); len(got) != 1 {
		t.Errorf("foldedGlobs(.env) = %q, want one cwd-anchored glob", got)
	}
	if n := globNeedle(".env", "/home/u/"); n != ".env" {
		t.Errorf("globNeedle(.env) = %q, want .env", n)
	}
	if n := globNeedle("//etc/passwd", "/home/u/"); n == "" {
		t.Error("globNeedle(absolute) lost its needle")
	}
}

func TestSudoDeniedOnlyForWholeSudoRows(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for deny, want := range map[string]bool{
		"Bash(sudo:*)": true, "Bash(sudo *)": true, "Bash(sudo)": true, "Bash(sudo rm *)": false, "Bash(sudoedit *)": false,
	} {
		body := `{"permissions":{"deny":["` + deny + `"]}}`
		if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		policyCache = paths.NewStampCache[policy]("safety.policy")
		if got := sudoDenied(); got != want {
			t.Errorf("%s: sudoDenied = %v, want %v", deny, got, want)
		}
	}
}

func TestBareEditDenyMatchesEveryPath(t *testing.T) {
	globs := foldedGlobs(parsePolicy(settings.PermissionLists{Deny: []string{"Edit"}}).editDeny, "/w")
	if !matchesProtected("/anywhere/deep/file.txt", globs) {
		t.Errorf("bare Edit deny globs %q miss an arbitrary path", globs)
	}
}

type stateWallCase struct {
	name, tool, agentID, dispatchID, depth, allow string
	parent                                        string // the dispatch record's parent; "-" writes no record
	want                                          core.Decision
}

var stateWallCases = []stateWallCase{
	{"leaf bash write", "Bash", "a1", "", "", "", "-", core.Block},
	{"main bash write", "Bash", "", "", "", "", "-", core.Allow},
	{"main-started grant", "Bash", "", "d1", "0", "Write(/state/harness/dispatch/**)", "", core.Allow},
	{"main-started bash grant", "Bash", "", "d1", "0", "Bash(tee:*)", "", core.Allow},
	{"main-started grant elsewhere", "Bash", "", "d1", "0", "Write(/state/harness/pi/**)", "", core.Block},
	{"main-started bare allow", "Bash", "", "d1", "0", "Write", "", core.Block},
	{"nested grant", "Bash", "", "d2", "1", "Write(/state/harness/dispatch/**)", "d1", core.Block},
	{"carried child grant", "Bash", "", "d2", "0", "Write(/state/harness/dispatch/**)", "d1", core.Block},
	{"no record grant", "Bash", "", "d3", "0", "Write(/state/harness/dispatch/**)", "-", core.Block},
	{"leaf edit tool", "Edit", "", "d1", "0", "", "", core.Block},
	{"main-started edit grant", "Edit", "", "d1", "0", "Edit(/state/harness/dispatch/**)", "", core.Allow},
}

// setStateWallDispatch exports c's dispatch env and writes its record (none for parent "-").
func setStateWallDispatch(t *testing.T, c stateWallCase) {
	t.Setenv("HARNESS_DISPATCH_ID", c.dispatchID)
	t.Setenv("HARNESS_DISPATCH_DEPTH", c.depth)
	t.Setenv("HARNESS_DISPATCH_PERMISSIONS", "")
	_ = os.RemoveAll(paths.DispatchDir())
	if depth, _ := strconv.Atoi(c.depth); c.parent != "-" {
		if err := (agent.Record{ID: c.dispatchID, Parent: c.parent, Depth: depth}).Save(); err != nil {
			t.Fatal(err)
		}
	}
	if c.dispatchID == "" {
		return
	}
	var p permission.Permissions
	if c.allow != "" {
		r, err := permission.ParseRule(c.allow)
		if err != nil {
			t.Fatal(err)
		}
		p.Allow = []permission.Rule{r}
	}
	raw, _ := json.Marshal(p)
	t.Setenv("HARNESS_DISPATCH_PERMISSIONS", string(raw))
}

func TestLeafStateWall(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	target := filepath.Join(cfg, "state", "harness", "dispatch", "x.json")
	for _, c := range stateWallCases {
		t.Run(c.name, func(t *testing.T) {
			setStateWallDispatch(t, c)
			input := map[string]string{"command": "tee " + target}
			if c.tool == "Edit" {
				input = map[string]string{"file_path": target, "old_string": "a", "new_string": "b"}
			}
			payload, _ := json.Marshal(map[string]any{"tool_name": c.tool, "cwd": "/tmp", "agent_id": c.agentID, "tool_input": input})
			in := core.ParseInput(bytes.NewReader(payload))
			r := Scan(in)
			if c.tool == "Edit" {
				r = protectedEdit(in)
			}
			if r.Decision != c.want || c.want == core.Block && !r.Floor {
				t.Fatalf("decision %v floor %v, want %v (floor): %s", r.Decision, r.Floor, c.want, r.Reason)
			}
		})
	}
}

// The test name stays free of the word the mayTouchState text test looks for: t.TempDir paths carry it.
func TestLeafWallUnnamedTarget(t *testing.T) {
	cfg, err := filepath.EvalSymlinks(t.TempDir()) // the state glob is unresolved; macOS temp sits under /var -> /private
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	setStateWallDispatch(t, stateWallCases[0])
	link := filepath.Join(t.TempDir(), "s")
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.StateDir(), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, c := range []struct {
		cwd, cmd string
		want     bool
	}{
		{"/tmp", "echo x > " + filepath.Join(link, "x"), true},
		{filepath.Dir(paths.StateDir()), "echo x > harness/x", true},
		{"/tmp", "rm " + filepath.Join(link, "x"), true},
		{paths.StateDir(), "rm x", true},
		{paths.StateDir(), "touch x", true},
		{cfg, "ruby -e IO.write(%q(" + filepath.Join(cfg, "STATE", "harness", "x") + "),%q(y))", paths.FoldsCase},
		{cfg, "echo x > ordinary/x", false},
		{cfg, "rm ordinary/x", false},
		{cfg, "git log -1", false},
	} {
		if got := mayTouchState(c.cmd, c.cwd); got != c.want {
			t.Errorf("mayTouchState(%q, %q) = %v, want %v", c.cmd, c.cwd, got, c.want)
		}
		payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": c.cwd, "agent_id": "a1",
			"tool_input": map[string]string{"command": c.cmd}})
		if r := Scan(core.ParseInput(bytes.NewReader(payload))); (r.Decision == core.Block && r.Floor) != c.want {
			t.Errorf("%q in %s: decision %v floor %v, want floor block %v", c.cmd, c.cwd, r.Decision, r.Floor, c.want)
		}
	}
}

func TestLeafWallReachabilityControls(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	setStateWallDispatch(t, stateWallCases[0])
	upper := filepath.Join(cfg, "STATE", "harness", "x")
	for _, c := range []struct {
		cwd, cmd string
		block    bool
	}{
		{paths.StateDir(), "git log -1", false},
		{paths.StateDir(), "touch " + filepath.Join(cfg, "ordinary"), false},
		{paths.StateDir(), "touch -r x " + filepath.Join(cfg, "ordinary"), false},
		{paths.StateDir(), "touch -ar x " + filepath.Join(cfg, "ordinary"), false},
		{paths.StateDir(), "touch -r /tmp/ref x", true},
		{paths.StateDir(), "touch -r/tmp/ref x", true},
		{paths.StateDir(), "touch -- -x", true},
		{cfg, "touch x", false},
		{cfg, "ruby <<EOF\nIO.write(%q(/tmp/out.txt),%q(" + upper + "))\nEOF", false},
		{cfg, "ruby <<EOF\nIO.write(%q(" + upper + "),%q(y))\nEOF", paths.FoldsCase},
		{cfg, "echo IO.write(%q(" + upper + "),%q(y))", false},
	} {
		payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": c.cwd, "agent_id": "a1",
			"tool_input": map[string]string{"command": c.cmd}})
		r := Scan(core.ParseInput(bytes.NewReader(payload)))
		if (r.Decision == core.Block && r.Floor) != c.block {
			t.Errorf("%q in %s: decision %v floor %v, want floor block %v: %s", c.cmd, c.cwd, r.Decision, r.Floor, c.block, r.Reason)
		}
	}
}
