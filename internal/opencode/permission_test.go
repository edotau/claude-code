package opencode

import (
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
		{"default mode denies unmatched", agent.Permissions{Mode: "default"}, `{"*":"deny"}`},
		{"bash prefix deny after allow", agent.Permissions{Mode: "auto", Allow: rules(t, "Bash"), Deny: rules(t, "Bash(rm *)", "Bash(git push:*)")},
			`{"*":"allow","bash":{"*":"allow","rm *":"deny","git push *":"deny"}}`},
		{"ask becomes deny", agent.Permissions{Mode: "auto", Ask: rules(t, "Bash(git log:*)")}, `{"*":"allow","bash":{"git log *":"deny"}}`},
		{"deny outranks allow on the same pattern", agent.Permissions{Allow: rules(t, "Read"), Deny: rules(t, "Read")}, `{"*":"deny","read":"deny"}`},
		{"read relative deny", agent.Permissions{Mode: "auto", Deny: rules(t, "Read(.env)")}, `{"*":"allow","read":{".env":"deny","*/.env":"deny"}}`},
		{"edit and write share edit", agent.Permissions{Allow: rules(t, "Edit", "Write")}, `{"*":"deny","edit":"allow"}`},
		{"tool level keys", agent.Permissions{Allow: rules(t, "Glob", "Grep", "WebFetch", "Agent"), Deny: rules(t, "Agent(Explore)")},
			`{"*":"deny","glob":"allow","grep":"allow","task":{"*":"allow","Explore":"deny"},"webfetch":"allow"}`},
		{"double star deny", agent.Permissions{Deny: rules(t, "Read(**/.env)")}, `{"*":"deny","read":{"*.env":"deny"}}`},
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

func TestOpencodePermissionRefusesWhatItCannotExpress(t *testing.T) {
	for _, r := range []string{"WebFetch(domain:evil.com)", "NotebookEdit", "Glob(src/**)"} {
		_, err := opencodePermission(agent.Permissions{Deny: rules(t, r)})
		if err == nil || !strings.Contains(err.Error(), r) {
			t.Errorf("deny %s: err = %v, want it to name the rule", r, err)
		}
	}
	if _, err := opencodePermission(agent.Permissions{Allow: rules(t, "Read(src/*.go)")}); err == nil {
		t.Error("an allow whose * would cross directories must refuse")
	}
}

func TestOpencodeApplyPermissions(t *testing.T) {
	env := []string{"A=1"}
	got, err := opencodeApplyPermissions(env, agent.RunOptions{Tools: true})
	if err != nil || len(got) != 1 || got[0] != "A=1" {
		t.Errorf("empty policy changed env: %v, %v", got, err)
	}
	req := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Mode: "auto", Deny: rules(t, "Bash(rm *)")}}
	got, err = opencodeApplyPermissions([]string{opencodeConfigEnv + `={"model":"m"}`}, req)
	perm := `{"*":"allow","bash":{"rm *":"deny"}}`
	if err != nil || got[0] != opencodeConfigEnv+`={"model":"m","permission":`+perm+`}` || got[1] != opencodePermissionEnv+"="+perm {
		t.Errorf("got %v, %v", got, err)
	}
	req.Tools = false
	if got, _ = opencodeApplyPermissions(env, req); len(got) != 1 {
		t.Errorf("tools-off keeps its own deny, got %v", got)
	}
}
