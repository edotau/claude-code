package opencode

import (
	"errors"
	"github.com/edotau/claude-code/internal/agent/retry"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
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
