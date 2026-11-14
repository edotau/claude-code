package memory_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// permissions is the conversion ask.memoryRunOptions applies to a Scope: plan mode, bank writes, no redirects.
func permissions(s memory.Scope) agent.Permissions {
	p := agent.Permissions{Mode: "plan", Deny: []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}}}
	for _, tool := range []string{"Edit", "Write"} {
		for _, d := range s.NoWrite {
			p.Deny = append(p.Deny, agent.Rule{Tool: tool, Arg: d})
		}
		for _, w := range s.Write {
			p.Allow = append(p.Allow, agent.Rule{Tool: tool, Arg: w})
		}
	}
	for _, d := range s.CdDirs {
		p.Allow = append(p.Allow, agent.CdRule(d))
	}
	for _, b := range s.Bash {
		p.Allow = append(p.Allow, agent.Rule{Tool: "Bash", Arg: b})
	}
	return p
}

func TestJobScopeBankAndDreamWrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	work := t.TempDir()
	store := memory.StoreDir()
	bank := memory.Dir(work)
	home := memory.HomeBankDir()
	claudeMD := filepath.Join(paths.ConfigDir(), "CLAUDE.md")
	other := filepath.Join(store, "other-repo", "progress.md")
	tests := []struct {
		kind, tool, arg string
		want            agent.Decision
	}{
		{memory.JobUpdate, "Write", filepath.Join(bank, "progress.md"), agent.Allow},
		{memory.JobUpdate, "Edit", filepath.Join(home, "profile.md"), agent.Allow},
		{memory.JobUpdate, "Write", other, agent.Deny},
		{memory.JobUpdate, "Write", claudeMD, agent.Deny},
		{memory.JobUpdate, "Write", filepath.Join(store, ".git", "config"), agent.Deny},
		{memory.JobUpdate, "Bash", "claude-code memory promote x", agent.Deny},
		{memory.JobUpdate, "Bash", "cd " + bank, agent.Allow},
		{memory.JobUpdate, "Bash", "cd " + store, agent.Deny},
		{memory.JobDream, "Write", other, agent.Allow},
		{memory.JobDream, "Edit", claudeMD, agent.Allow},
		{memory.JobDream, "Write", filepath.Join(store, ".git", "config"), agent.Deny},
		{memory.JobDream, "Write", filepath.Join(t.TempDir(), "x.md"), agent.Deny},
		{memory.JobDream, "Bash", "claude-code memory promote --to claude-md x", agent.Allow},
		{memory.JobDream, "Bash", "rm -rf " + store, agent.Deny},
		{memory.JobDream, "Bash", "echo x > " + other, agent.Deny},
	}
	for _, tc := range tests {
		p := permissions(memory.JobScope(tc.kind, work))
		if got := p.Decide(tc.tool, tc.arg); got != tc.want {
			t.Errorf("%s: Decide(%s, %q) = %v, want %v", tc.kind, tc.tool, strings.TrimPrefix(tc.arg, store), got, tc.want)
		}
	}
}

func TestJobScopeDigestAndUnknownHoldNoTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, kind := range []string{memory.JobDigest, "nonsense", ""} {
		s := memory.JobScope(kind, t.TempDir())
		if s.Tools || len(s.Write)+len(s.Bash)+len(s.CdDirs) != 0 {
			t.Errorf("JobScope(%q) = %+v, want no tools and no grants", kind, s)
		}
	}
	if s := memory.JobScope(memory.JobUpdate, t.TempDir()); !s.Tools {
		t.Errorf("update scope reports no tools: %+v", s)
	}
}
