package safety

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/settings"
)

func policyTemplateFile(tb testing.TB) {
	tb.Helper()
	tb.Setenv("CLAUDE_CONFIG_DIR", tb.TempDir())
	if err := os.WriteFile(settings.SettingsFile(), settings.Template(), 0o600); err != nil {
		tb.Fatal(err)
	}
}

func TestPolicySizedReadTemplate(t *testing.T) {
	policyTemplateFile(t)
	lists, err := settings.LivePermissions()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if want := baselineParsePolicy(lists); !reflect.DeepEqual(got, want) {
		t.Fatalf("policy differs from template: got %#v; want %#v", got, want)
	}
}

func TestPolicyReadValidationEquivalent(t *testing.T) {
	policyTemplateFile(t)
	const valid = `{"permissions":{"deny":[]}}`
	for _, body := range []string{
		`null`, `{}`, `{"permissions":null}`, `{"permissions":{}}`,
		`{"permissions":{"deny":null}}`, `{"permissions":"x"}`, `{"permissions":{"deny":"x"}}`,
		`{"permissions":{"deny":[1]}}`, `{"permissions":{"deny":[],"allow":1}}`,
		`{"permissions":{"deny":[],"ask":false}}`, `{bad`, valid + `{}`, valid,
		`{"permissions":{"deny":[],"deny":["Bash","Edit(x)"]}}`,
		`{"permissions":{"deny":["Bash"],"deny":null}}`,
		valid + strings.Repeat(" ", (4<<20)-len(valid)),
		valid + strings.Repeat(" ", (4<<20)+1-len(valid)),
	} {
		if err := os.WriteFile(settings.SettingsFile(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		lists, beforeErr := settings.LivePermissions()
		got, afterErr := readPolicy()
		if (beforeErr == nil) != (afterErr == nil) {
			t.Fatalf("length %d: baseline %v; sized %v", len(body), beforeErr, afterErr)
		}
		if beforeErr != nil {
			if beforeErr.Error() != afterErr.Error() ||
				errors.Is(beforeErr, settings.ErrTooLarge) != errors.Is(afterErr, settings.ErrTooLarge) {
				t.Errorf("baseline %v; sized %v", beforeErr, afterErr)
			}
		} else if want := baselineParsePolicy(lists); !reflect.DeepEqual(got, want) {
			t.Errorf("length %d: got %#v; want %#v", len(body), got, want)
		}
	}
}

func TestPolicyParseEquivalent(t *testing.T) {
	for _, lists := range []settings.PermissionLists{
		{},
		{Allow: []string{"Read", "Edit", "broken("}, Ask: []string{"Write(~/raw)", "Edit( )"}},
		{Deny: []string{"Bash", "Bash(echo *)", "Edit", "Write", "Edit()", "Write( /x )", "Read(x)"}},
	} {
		if got, want := parsePolicy(lists), baselineParsePolicy(lists); !reflect.DeepEqual(got, want) {
			t.Errorf("%#v: got %#v; want %#v", lists, got, want)
		}
	}
}

// baselineParsePolicy preserves the pre-3.21 parser for output and allocation comparisons.
func baselineParsePolicy(l settings.PermissionLists) policy {
	var p policy
	for _, set := range []struct {
		in   []string
		bash *[]permission.Rule
		edit *[]string
	}{{l.Allow, &p.bashAllow, nil}, {l.Ask, &p.bashAsk, &p.editAsk}, {l.Deny, &p.bashDeny, &p.editDeny}} {
		for _, s := range set.in {
			if r, err := permission.ParseRule(s); err == nil && r.Tool == "Bash" {
				*set.bash = append(*set.bash, r)
				continue
			}
			if set.edit != nil {
				*set.edit = append(*set.edit, baselineEditGlobs(s)...)
			}
		}
	}
	return p
}

// baselineEditGlobs preserves the pre-3.21 editGlobs.
func baselineEditGlobs(row string) []string {
	if row == "Edit" || row == "Write" { // a bare tool row covers every path
		return []string{"**/*"}
	}
	for _, tool := range []string{"Edit(", "Write("} {
		g, ok := strings.CutPrefix(row, tool)
		if !ok || !strings.HasSuffix(g, ")") {
			continue
		}
		if g = strings.TrimSpace(strings.TrimSuffix(g, ")")); g != "" {
			return []string{g}
		}
	}
	return nil
}

func BenchmarkPolicyRead(b *testing.B) {
	policyTemplateFile(b)
	for _, tc := range []struct {
		name string
		read func() (policy, error)
	}{
		{"baseline", func() (policy, error) {
			lists, err := settings.LivePermissions()
			return baselineParsePolicy(lists), err
		}},
		{"parsed", func() (policy, error) {
			lists, err := settings.LivePermissions()
			return parsePolicy(lists), err
		}},
		{"sized", readPolicy},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := tc.read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

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

// leafScan is Scan for leaf a1's Bash cmd in cwd: whether it is a floor block.
func leafScan(cwd, cmd string) (bool, string) {
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cwd, "agent_id": "a1",
		"tool_input": map[string]string{"command": cmd}})
	r := Scan(core.ParseInput(bytes.NewReader(payload)))
	return r.Decision == core.Block && r.Floor, r.Reason
}

// touch, ln and target-dir copies reach the dir from any cwd, through env -C, rtk, a cd or a symlink.
func TestLeafWallOperandWrites(t *testing.T) {
	cfg, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	setStateWallDispatch(t, stateWallCases[0])
	sd := paths.StateDir() // <cfg>/state/harness
	link := filepath.Join(t.TempDir(), "s")
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sd, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, c := range []struct {
		cwd, cmd string
		block    bool
	}{
		{cfg, "touch state/harness/x", true},
		{"/tmp", "touch " + link + "/x", true},
		{"/tmp", "cd " + link + " && touch x", true},
		{cfg, "env -C state/harness touch x", true},
		{cfg, "rtk touch state/harness/x", true},
		{cfg, "touch state/x", false},
		{"/tmp", `cd "$D" && touch ` + sd + "/x", true},
		{sd, "ln -s /tmp/evil harness/x", true},
		{"/tmp", "ln -sf /tmp/evil " + sd + "/harness/dispatch/d1.json", true},
		{"/tmp", "ln -s /tmp/evil " + link, true},
		{sd, "cp --target-directory harness /tmp/x", true},
		{sd, "cp -t. /tmp/x", true},
		{sd, "install -d sub", true},
		{cfg, "touch ordinary/x", false},
		{"/tmp", `cd "$D" && touch x`, false},
		{"/tmp", "ln -s " + sd + "/harness/x /tmp/l", false},
		{"/tmp", "ln -s a b", false},
		{"/tmp", "cp -t /tmp/out " + sd + "/x", false},
		{sd, "install -d -m 755 /tmp/x", false},
	} {
		if got, reason := leafScan(c.cwd, c.cmd); got != c.block {
			t.Errorf("%q in %s: floor block %v, want %v: %s", c.cmd, c.cwd, got, c.block, reason)
		}
	}
}

// A config dir spelled through a symlink still walls a cwd or target already in its resolved form.
func TestLeafWallResolvedConfigSpelling(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.Symlink(real, cfg); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	resolved := filepath.Join(real, "state", "harness")
	if err := os.MkdirAll(filepath.Join(resolved, "dispatch"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		allow, cmd string
		block      bool
	}{
		{"", "echo x > y", true},
		{"", "tee y", true},
		{"", "cp /tmp/a y", true},
		{"", "git log -1", false},
		{"Write(/state/harness/dispatch/**)", "tee dispatch/y", false},
		{"Write(/state/harness/dispatch/**)", "tee y", true},
	} {
		setStateWallDispatch(t, stateWallCase{dispatchID: "d1", depth: "0", allow: c.allow})
		payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": resolved,
			"tool_input": map[string]string{"command": c.cmd}})
		r := Scan(core.ParseInput(bytes.NewReader(payload)))
		if got := r.Decision == core.Block && r.Floor; got != c.block {
			t.Errorf("%q (allow %q): floor block %v, want %v: %s", c.cmd, c.allow, got, c.block, r.Reason)
		}
	}
	setStateWallDispatch(t, stateWallCase{dispatchID: "d1", depth: "0"})
	payload, _ := json.Marshal(map[string]any{"tool_name": "Edit", "cwd": "/tmp",
		"tool_input": map[string]string{"file_path": filepath.Join(resolved, "x"), "old_string": "a", "new_string": "b"}})
	if r := protectedEdit(core.ParseInput(bytes.NewReader(payload))); r.Decision != core.Block {
		t.Errorf("Edit of the resolved spelling: decision %v, want block", r.Decision)
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
