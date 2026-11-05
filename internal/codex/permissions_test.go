package codex

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

func TestCodexRender(t *testing.T) {
	bash := func(a string) agent.Rule { return agent.Rule{Tool: "Bash", Arg: a} }
	cases := []struct {
		name    string
		p       agent.Permissions
		tools   bool
		rules   string
		approve bool
		err     string
	}{
		{name: "deny prefix", p: agent.Permissions{Mode: "auto", Deny: []agent.Rule{bash("rm:*")}}, tools: true,
			rules: `prefix_rule(pattern=["rm"], decision="forbidden"`, approve: true},
		{name: "allow multi-word", p: agent.Permissions{Allow: []agent.Rule{bash("git status:*")}},
			rules: `pattern=["git", "status"], decision="allow"`},
		{name: "ask renders prompt", p: agent.Permissions{Ask: []agent.Rule{bash("git push:*")}},
			rules: `decision="prompt"`, approve: true},
		{name: "exact deny over-matches safely", p: agent.Permissions{Deny: []agent.Rule{bash("rm -rf /")}},
			rules: `pattern=["rm", "-rf", "/"]`},
		{name: "exact allow skipped", p: agent.Permissions{Allow: []agent.Rule{bash("ls")}}},
		{name: "write deny covered by read-only", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "src/**"}}}},
		{name: "write deny refused with tools", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "src/**"}}}, tools: true,
			err: "Edit(src/**)"},
		{name: "read deny refused", p: agent.Permissions{Deny: []agent.Rule{{Tool: "Read", Arg: ".env"}}}, err: "Read(.env)"},
		{name: "pipeline deny refused", p: agent.Permissions{Deny: []agent.Rule{bash("cat x | wc")}}, err: "Bash(cat x | wc)"},
		{name: "substitution deny refused", p: agent.Permissions{Deny: []agent.Rule{bash("echo $(id):*")}}, err: "command prefix"},
		{name: "plan with tools refused", p: agent.Permissions{Mode: "plan"}, tools: true, err: "plan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := codexRender(c.p, c.tools)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want it to name %q", err, c.err)
				}
				return
			}
			if err != nil || got.approve != c.approve || (c.rules == "") != (got.rules == "") || !strings.Contains(got.rules, c.rules) {
				t.Fatalf("got %+v err %v, want rules containing %q approve %v", got, err, c.rules, c.approve)
			}
		})
	}
}

func TestCodexApplyPermissionsArgv(t *testing.T) {
	argv := []string{"exec", "-c", "x=1", "--skip-git-repo-check", "--sandbox", "workspace-write", "-"}
	env := []string{"A=1"}
	gotArgv, gotEnv, rm, err := codexApplyPermissions(agent.RunOptions{}, argv, env)
	defer rm()
	if err != nil || strings.Join(gotArgv, " ") != strings.Join(argv, " ") || len(gotEnv) != 1 {
		t.Fatalf("zero Permissions changed the run: %v %v %v", gotArgv, gotEnv, err)
	}
	req := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Mode: "auto"}}
	gotArgv, _, rm2, err := codexApplyPermissions(req, argv, env)
	defer rm2()
	if want := "exec -c x=1 --skip-git-repo-check --approve-for-me -"; err != nil || strings.Join(gotArgv, " ") != want {
		t.Fatalf("auto argv = %q (%v), want %q", gotArgv, err, want)
	}
}
