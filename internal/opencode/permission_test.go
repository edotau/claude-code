package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hooks/safety"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func rules(t *testing.T, ss ...string) []agent.Rule {
	t.Helper()
	var out []agent.Rule
	for _, s := range ss {
		r, err := agent.ParseRule(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestOpencodePermissionGolden(t *testing.T) {
	cases := []struct {
		name string
		p    agent.Permissions
		want string
	}{
		{"mode auto alone", agent.Permissions{Mode: "auto"}, `{"*":"allow"}`},
		{"default mode asks unmatched", agent.Permissions{Mode: "default"}, `{"*":"ask"}`},
		{"bash allow prefix", agent.Permissions{Mode: "auto", Allow: rules(t, "Bash", "Bash(git log:*)")},
			`{"*":"allow","bash":{"*":"allow","git log *":"allow"}}`},
		{"ask stays ask", agent.Permissions{Mode: "auto", Ask: rules(t, "Bash(git log:*)")}, `{"*":"allow","bash":{"git log *":"ask"}}`},
		{"default mode asks", agent.Permissions{Deny: rules(t, "Read(.env)")}, `{"*":"ask","read":{".env":"deny","*/.env":"deny"}}`},
		{"plan mode is read-only", agent.Permissions{Mode: "plan"},
			`{"*":"deny","bash":"deny","edit":"deny","glob":"allow","grep":"allow","list":"allow","read":"allow","task":"deny","webfetch":"allow","websearch":"allow"}`},
		{"plan mode keeps a read deny and a write allow", agent.Permissions{Mode: "plan", Allow: rules(t, "Edit"), Deny: rules(t, "Read(.env)")},
			`{"*":"deny","bash":"deny","edit":"allow","glob":"allow","grep":"allow","list":"allow","read":{"*":"allow",".env":"deny","*/.env":"deny"},"task":"deny","webfetch":"allow","websearch":"allow"}`},
		{"plan mode preserves explicit write grants", agent.Permissions{Mode: "plan", Allow: rules(t, "Bash(go test:*)", "Write"), Ask: rules(t, "Agent")},
			`{"*":"deny","bash":{"*":"deny","go test *":"allow"},"edit":"allow","glob":"allow","grep":"allow","list":"allow","read":"allow","task":"ask","webfetch":"allow","websearch":"allow"}`},
		{"deny outranks allow on the same pattern", agent.Permissions{Allow: rules(t, "Read"), Deny: rules(t, "Read")}, `{"*":"ask","read":"deny"}`},
		{"read relative deny", agent.Permissions{Mode: "auto", Deny: rules(t, "Read(.env)")}, `{"*":"allow","read":{".env":"deny","*/.env":"deny"}}`},
		{"edit and write share edit", agent.Permissions{Allow: rules(t, "Edit", "Write")}, `{"*":"ask","edit":"allow"}`},
		{"tool level keys", agent.Permissions{Allow: rules(t, "Glob", "Grep", "WebFetch", "Agent"), Deny: rules(t, "Agent(Explore)")},
			`{"*":"ask","glob":"allow","grep":"allow","task":{"*":"allow","Explore":"deny"},"webfetch":"allow"}`},
		{"double star deny", agent.Permissions{Deny: rules(t, "Read(**/.env)")}, `{"*":"ask","read":{"*.env":"deny"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := opencodePermission(c.p)
			if err != nil || string(got) != c.want {
				t.Errorf("got %s, %v\nwant %s", got, err, c.want)
			}
		})
	}
}

// A Bash Deny renders natively again (the plugin's permission-gate covers `bash -c`); only --safe-mode refuses it.
func TestOpencodePermissionBashDeny(t *testing.T) {
	got, err := opencodePermission(agent.Permissions{Mode: "auto", Deny: rules(t, "Bash(rm *)")})
	if want := `{"*":"allow","bash":{"rm *":"deny"}}`; err != nil || string(got) != want {
		t.Errorf("got %s, %v; want %s", got, err, want)
	}
	req := agent.RunOptions{Tools: true, SafeMode: true, Permissions: agent.Permissions{Deny: rules(t, "Bash(rm *)")}}
	_, err = opencodeApplyPermissions(nil, req) // gate hooks are bound: the refusal names them and the way out
	requireFatal(t, err, "safety-scan", "without --safe-mode")
	orig := bindHooks
	t.Cleanup(func() { bindHooks = orig })
	bindHooks = func() []hooks.Bound { return nil }
	_, err = opencodeApplyPermissions(nil, req) // no gate bound: the Bash deny alone is refused
	requireFatal(t, err, "Bash(rm *)")
}

// requireFatal fails unless err is a fatal agent error containing every want.
func requireFatal(t *testing.T, err error, wants ...string) {
	t.Helper()
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal {
		t.Fatalf("err = %v, want a fatal agent.Error", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err = %v, want it to contain %q", err, w)
		}
	}
}

func TestOpencodePermissionRefusesWhatItCannotExpress(t *testing.T) {
	for _, r := range []string{"WebFetch(domain:evil.com)", "NotebookEdit", "Glob(src/**)"} {
		_, err := opencodePermission(agent.Permissions{Deny: rules(t, r)})
		if err == nil || !strings.Contains(err.Error(), "cannot express deny "+r) {
			t.Errorf("deny %s: err = %v, want it to name the rule", r, err)
		}
	}
	if _, err := opencodePermission(agent.Permissions{Allow: rules(t, "Read(src/*.go)")}); err == nil || !strings.Contains(err.Error(), "cannot express allow") {
		t.Error("an allow whose * would cross directories must refuse")
	}
}

// A tool opencode has no permission for is refused in every set: plan mode never swallows it as a skipped rule.
func TestOpencodePermissionRefusesUnknownToolInPlan(t *testing.T) {
	r := rules(t, "NotebookEdit")
	for name, p := range map[string]agent.Permissions{
		"allow": {Mode: "plan", Allow: r}, "ask": {Mode: "plan", Ask: r}, "deny": {Mode: "plan", Deny: r},
	} {
		if _, err := opencodePermission(p); err == nil || !strings.Contains(err.Error(), "NotebookEdit") {
			t.Errorf("plan %s NotebookEdit: err = %v, want a refusal naming the tool", name, err)
		}
	}
}

func TestOpencodeApplyPermissions(t *testing.T) {
	env := []string{"A=1"}
	got, err := opencodeApplyPermissions(env, agent.RunOptions{Tools: true})
	if err != nil || len(got) != 1 || got[0] != "A=1" {
		t.Errorf("empty policy changed env: %v, %v", got, err)
	}
	req := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Mode: "auto", Deny: rules(t, "Read(.env)")}}
	got, err = opencodeApplyPermissions([]string{opencodeConfigEnv + `={"model":"m"}`}, req)
	perm := `{"*":"allow","read":{".env":"deny","*/.env":"deny"}}`
	if err != nil || got[0] != opencodeConfigEnv+`={"model":"m","permission":`+perm+`}` || got[1] != opencodePermissionEnv+"="+perm {
		t.Errorf("got %v, %v", got, err)
	}
	req.Tools = false
	if got, _ = opencodeApplyPermissions(env, req); len(got) != 1 {
		t.Errorf("tools-off keeps its own deny, got %v", got)
	}
}

func TestOpencodeReadOnlyLaunch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hub, safe bool
	}{{"cli", false, false}, {"hub", true, false}, {"cli safe mode", false, true}, {"hub safe mode", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			engine, seen := readOnlyLaunchEngine(t, tc.hub)
			if engine.Caps().ReadOnly != agent.Emulated {
				t.Fatal("read-only capability must be emulated")
			}
			req := agent.RunOptions{Prompt: "--auto", Tools: true, ReadOnly: true, Autonomous: true, SafeMode: tc.safe}
			res, err := agent.Run(t.Context(), engine, req, wire.Discard, nil)
			if tc.safe {
				assertReadOnlyRefused(t, seen, err)
				return
			}
			if err != nil || res.Answer != "ok" || len(res.Dropped) != 0 {
				t.Fatalf("Run = %+v, %v; want tools-on read-only success without dropped flags", res, err)
			}
			assertReadOnlyLaunch(t, seen)
		})
	}
}

func assertReadOnlyRefused(t *testing.T, seen string, err error) {
	t.Helper()
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(err.Error(), "cannot enforce deny Bash") {
		t.Fatalf("safe-mode error = %v, want fatal Bash refusal", err)
	}
	if _, err := os.Stat(seen); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("safe mode launched a child: %v", err)
	}
}

func readOnlyLaunchEngine(t *testing.T, hub bool) (agent.Engine, string) {
	t.Helper()
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	writeAuthJSON(t, data, `{"opencode":{"type":"api","key":"k"}}`)
	orig := bindHooks
	bindHooks = func() []hooks.Bound { return nil }
	t.Cleanup(func() { bindHooks = orig })
	seen := filepath.Join(t.TempDir(), "launch")
	pathstest.FakeVendor(t, "opencode", `{
printf 'ARG=%s\n' "$@"
printf 'PERM=%s\nCFG=%s\nDISPATCH=%s\n' "$OPENCODE_PERMISSION" "$OPENCODE_CONFIG_CONTENT" "$HARNESS_DISPATCH_PERMISSIONS"
} > '`+seen+`'
printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	engine, err := agent.Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if hub {
		p := &providers.Provider{Name: "p", OpencodeID: "opencode", Models: map[string]string{models.Opus: "m"}}
		engine = hubDispatch{agent: "opencode", provider: p, reg: hubRegistry()}
	}
	return engine, seen
}

func assertReadOnlyLaunch(t *testing.T, seen string) {
	t.Helper()
	values := map[string]string{}
	var argv []string
	for line := range strings.SplitSeq(pathstest.ReadFile(t, seen), "\n") {
		k, v, _ := strings.Cut(line, "=")
		if k == "ARG" {
			argv = append(argv, v)
		} else {
			values[k] = v
		}
	}
	if len(argv) < 2 || slices.Contains(flagsOnly(argv), "--auto") || !slices.Equal(argv[len(argv)-2:], []string{"--", "--auto"}) {
		t.Fatalf("read-only argv must remove --auto flags but preserve the prompt: %q", argv)
	}
	assertReadOnlyPerm(t, values["PERM"], values["CFG"])
	if !strings.Contains(values["CFG"], "harness-hooks-") {
		t.Fatal("read-only launch lacks the permission-gate plugin")
	}
	assertReadOnlyDispatch(t, values["DISPATCH"])
}

func assertReadOnlyPerm(t *testing.T, perm, cfg string) {
	t.Helper()
	for _, want := range []string{`"edit":"deny"`, `"websearch":"allow"`, `"bash":{"*":"deny"`, `"git log *":"allow"`} {
		if !strings.Contains(perm, want) || !strings.Contains(cfg, want) {
			t.Fatalf("read-only must keep reads on, writes denied and bash on the list only (%s): %s", want, perm)
		}
	}
	if strings.Contains(perm, `"rm`) || strings.Contains(perm, `"bash":"allow"`) {
		t.Fatalf("read-only bash must not allow more than the list: %s", perm)
	}
}

func assertReadOnlyDispatch(t *testing.T, raw string) {
	t.Helper()
	var p agent.Permissions
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("dispatch permissions: %v", err)
	}
	for _, r := range agent.ReadOnlyDeny {
		if !slices.Contains(p.Deny, r) {
			t.Errorf("%s lacks deny %s: %+v", agent.DispatchPermissionsEnv, r, p)
		}
	}
}

func TestOpencodePolicyArgv(t *testing.T) {
	if got := opencodePolicyArgv([]string{"run"}, agent.RunOptions{Tools: true}); len(got) != 1 {
		t.Fatalf("no policy must leave argv unchanged: %v", got)
	}
}

// TestOpencodePolicyArgvKeepsPromptLast pins --auto before options, attachments and the dash-prefixed prompt.
func TestOpencodePolicyArgvKeepsPromptLast(t *testing.T) {
	pol := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Deny: rules(t, "Read(.env)")}}
	h := agent.Headless{Prompt: "-draft", Attach: []string{"notes.txt"}, Tools: true}
	argv := opencodeArgv("p/m", h)
	got := opencodePolicyArgv(argv, pol)
	want := []string{"run", "--auto", "--format", "json", "-m", "p/m", "--title", "opencode: -draft",
		"-f", "notes.txt", "--", "-draft"}
	if !slices.Equal(got, want) {
		t.Fatalf("policy argv = %v, want %v with the prompt last", got, want)
	}
}

func TestOpencodePolicyArgvAutoPrompt(t *testing.T) {
	req := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Deny: rules(t, "Read(.env)")}}
	argv := opencodeArgv("p/m", agent.Headless{Prompt: "--auto", Tools: true, Autonomous: false})
	before := slices.Clone(argv)
	got := opencodePolicyArgv(argv, req)
	want := append([]string{"run", "--auto"}, before[1:]...)
	if !slices.Equal(got, want) {
		t.Fatalf("prompt --auto must not suppress the policy flag: got %q, want %q", got, want)
	}
	if !slices.Equal(argv, before) {
		t.Fatalf("policy mutated input argv: got %q, want %q", argv, before)
	}
	if again := opencodePolicyArgv(got, req); !slices.Equal(again, got) {
		t.Fatalf("real --auto flag duplicated: %q", again)
	}
}

// TestOpencodeExecRefusesPolicyEnv: under a policy, --env may not set the names the policy pins (it would win, being
// last); with tools on and no policy --env passes through as before.
func TestOpencodeExecRefusesPolicyEnv(t *testing.T) {
	deny, err := agent.ParsePermissions("", nil, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		req  agent.RunOptions
		want string
	}{
		{"deny rule", agent.RunOptions{Tools: true, Permissions: deny, Env: []string{opencodePermissionEnv + `="allow"`}}, opencodePermissionEnv},
		{"tools off", agent.RunOptions{Env: []string{opencodeConfigEnv + `={"permission":"allow"}`}}, opencodeConfigEnv},
		{"safe mode", agent.RunOptions{Tools: true, SafeMode: true, Env: []string{opencodePermissionEnv + "=x"}}, opencodePermissionEnv},
		{"read only", agent.RunOptions{Tools: true, ReadOnly: true, Env: []string{opencodePermissionEnv + `="allow"`}}, opencodePermissionEnv},
		{"no policy", agent.RunOptions{Tools: true, Env: []string{opencodePermissionEnv + `="allow"`}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no opencode installed: a run that gets past the check stops at Bin
			_, err := (opencodeEngine{agent.NewBaseEngine("opencode")}).Exec(context.Background(), agent.CLIPrep{}, tc.req, wire.Discard)
			switch {
			case tc.want == "" && (err == nil || strings.Contains(err.Error(), "permission policy")):
				t.Fatalf("no policy: err = %v, want --env to pass the check (and the run to stop at the missing binary)", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "allow")):
				t.Fatalf("err = %v, want a refusal naming %s and not its value", err, tc.want)
			}
		})
	}
}

func TestOpencodePermissionResumeReloadsGrant(t *testing.T) {
	engine, seen := readOnlyLaunchEngine(t, false)
	for _, granted := range []bool{false, true} {
		p := agent.Permissions{Mode: "plan"}
		if granted {
			p.Allow = rules(t, "Bash(make lint)")
		}
		req := agent.RunOptions{Prompt: "permission probe", Tools: true, SessionID: "same-session", Permissions: p}
		res, err := agent.Run(t.Context(), engine, req, wire.Discard, nil)
		if err != nil || res.Answer != "ok" {
			t.Fatalf("Run: %+v, %v", res, err)
		}
		output := pathstest.ReadFile(t, seen)
		if !strings.Contains(output, "ARG=--session\nARG=same-session\n") {
			t.Fatalf("resume changed session: %s", output)
		}
		native, gate := launchPermissions(t, output)
		var bash map[string]string
		nativeAllows := json.Unmarshal(native["bash"], &bash) == nil && bash["make lint"] == "allow"
		if nativeAllows != granted || safety.GateBlocks(gate, "Bash", "make lint") == granted {
			t.Fatalf("native/plugin disagree, granted=%v native=%s gate=%+v", granted, native["bash"], gate)
		}
		if !safety.GateBlocks(gate, "Bash", "make deploy") {
			t.Fatal("exact grant widened to another command")
		}
	}
}

// launchPermissions reads the native permission map and the gate's dispatch permissions the fake launcher logged.
func launchPermissions(t *testing.T, output string) (native map[string]json.RawMessage, gate agent.Permissions) {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		if raw, ok := strings.CutPrefix(line, "PERM="); ok {
			if err := json.Unmarshal([]byte(raw), &native); err != nil {
				t.Fatal(err)
			}
		}
		if raw, ok := strings.CutPrefix(line, "DISPATCH="); ok {
			if err := json.Unmarshal([]byte(raw), &gate); err != nil {
				t.Fatal(err)
			}
		}
	}
	return native, gate
}
