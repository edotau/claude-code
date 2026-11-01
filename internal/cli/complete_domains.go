package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

// formatValues is the shared text|json domain almost every --format flag offers.
func formatValues() []string { return []string{helptext.Text, helptext.JSON} }

func setupWorkspaceSubwords() []string { return []string{"init", "link", "status"} }

func setupModes() []string { return []string{"adopt", "nuke", "workspace"} }

func cliAgentNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

// toolNames derives from agent.Tools, the install --tools roster.
func toolNames() []string { return tools.Names() }

// configureAgentNames are the agent CLIs `configure --agent` can scope to: the agents plus claude itself.
func configureAgentNames() []string { return append([]string{"claude"}, cliAgentNames()...) }

// hookEvents derives from the hook chain the registry actually declares — never a hand copy of it.
func hookEvents() []string {
	var out []string
	for _, s := range settings.Specs() {
		if !slices.Contains(out, s.Event) {
			out = append(out, s.Event)
		}
	}
	return out
}

func workflowPatterns() []string {
	return []string{"pipeline", "parallel", "evaluator", "orchestrator"}
}

// reviewToolNames derives from reviewTools (review.go) so a new detector is completable for free.
func reviewToolNames() []string {
	out := make([]string, 0, len(reviewTools))
	for n := range reviewTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// configureSections derives from configure.Sections, the package's own run order.
func configureSections() []string {
	return append(append([]string{}, configure.Sections...), "desktop")
}

// modelPinAliases offers "alias=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinAliases() []string {
	out := make([]string, 0, len(models.AliasNames)+1)
	for _, s := range models.AliasNames {
		out = append(out, s+"=")
	}
	return append(out, "all=")
}

// browserNames mirrors `network browser --setup --browser`'s accepted values.
func browserNames() []string {
	return []string{"chrome", "edge", "brave", "arc", "vivaldi", "chromium", "default"}
}

// providerNames reads the provider registry straight off disk (no network): the same file
// `claude-code configure provider --list`/`switch` already read.
func providerNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}

// agentNames lists the `ask --agent` agent names; agent.Engines() only builds the roster, it never
// dials an agent.
func agentNames() []string {
	var out []string
	for _, r := range agent.Engines() {
		out = append(out, r.Name())
	}
	return out
}

// secretNames unions every env var the harness itself reads (each enabled provider's auth.env, the
// setup-token) with the names index `vault set` has already written — never a hand copy of either.
func secretNames() []string {
	var out []string
	if reg, err := providers.Load(); err == nil {
		for _, n := range reg.Names() {
			if env := reg.Providers[n].Auth.Env; env != "" {
				out = append(out, env)
			}
		}
	}
	out = append(out, providers.OAuthTokenEnv)
	out = append(out, secretstore.Names()...)
	slices.Sort(out)
	return slices.Compact(out)
}

// memoryRepoNames lists the catalog's repo keys (dirs under memory/ with a valid origin record).
func memoryRepoNames() []string {
	ents, err := memstore.RepoDirs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Key)
	}
	return out
}

// worktreeNames lists the worktree dirs under this repo's ~/worktrees/<repo> root — a plain
// directory listing (no git-status classification), fast enough for the Tab path.
func worktreeNames() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	repo, err := worktree.ResolveRepo(cwd)
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(repo.Root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// webViewAgentIDs lists subagent ids from the current project's most recent sessions — a bounded
// directory glob (no transcript parsing), current session first, fast enough for the Tab path.
func webViewAgentIDs() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	projectDir := filepath.Join(paths.ConfigDir(), "projects", memstore.ProjectSlug(cwd))
	sessions, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	if err != nil || len(sessions) == 0 {
		return nil
	}
	modTime := make(map[string]time.Time, len(sessions)) // stat each session once, not once per comparison
	for _, s := range sessions {
		if fi, err := os.Stat(s); err == nil {
			modTime[s] = fi.ModTime()
		}
	}
	slices.SortFunc(sessions, func(a, b string) int {
		at, aok := modTime[a]
		bt, bok := modTime[b]
		if !aok || !bok {
			return 0
		}
		return bt.Compare(at)
	})
	const maxSessions = 2
	if len(sessions) > maxSessions {
		sessions = sessions[:maxSessions]
	}
	seen := map[string]bool{}
	var ids []string
	for _, s := range sessions {
		dir := strings.TrimSuffix(s, ".jsonl")
		patterns := []string{
			filepath.Join(dir, "subagents", "agent-*.jsonl"),
			filepath.Join(dir, "subagents", "workflows", "*", "agent-*.jsonl"),
		}
		for _, pattern := range patterns {
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".jsonl")
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}
