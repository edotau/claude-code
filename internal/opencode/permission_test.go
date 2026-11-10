package opencode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
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
			`{"*":"deny","bash":"deny","edit":"deny","glob":"allow","grep":"allow","list":"allow","read":"allow","task":"deny","webfetch":"allow"}`},
		{"plan mode keeps a read deny and ignores a write allow", agent.Permissions{Mode: "plan", Allow: rules(t, "Edit"), Deny: rules(t, "Read(.env)")},
			`{"*":"deny","bash":"deny","edit":"deny","glob":"allow","grep":"allow","list":"allow","read":{"*":"allow",".env":"deny","*/.env":"deny"},"task":"deny","webfetch":"allow"}`},
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
	_, err = opencodeApplyPermissions(nil, req)
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(err.Error(), "Bash(rm *)") {
		t.Errorf("--safe-mode err = %v, want a fatal agent.Error naming the rule", err)
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

func TestOpencodePolicyArgv(t *testing.T) {
	pol := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Deny: rules(t, "Read(.env)")}}
	if got := opencodePolicyArgv([]string{"run"}, pol); !slices.Equal(got, []string{"run", "--auto"}) {
		t.Fatalf("policy argv = %v, want --auto added", got)
	}
	if got := opencodePolicyArgv([]string{"run", "--auto"}, pol); len(got) != 2 {
		t.Fatalf("--auto duplicated: %v", got)
	}
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
