package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/dispatch"
	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tools"
	"github.com/edotau/claude-code/internal/worktree"
)

// formatValues is the shared text|json domain almost every --format flag offers.
func formatValues() []string { return []string{outfmt.Text, outfmt.JSON} }

func setupWorkspaceSubwords() []string { return []string{"init", "link", "status"} }

func setupModes() []string { return []string{"adopt", "nuke", "workspace"} }

func vendorNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

// toolNames derives from tools.Tools (internal/tools), the install --tools roster.
func toolNames() []string { return tools.Names() }

// configureAgentNames are the agent CLIs `configure --agent` can scope to: the vendors plus claude itself.
func configureAgentNames() []string { return append([]string{"claude"}, vendorNames()...) }

// hookNames derives from the hook chain the registry actually declares — never a hand copy of it.
func hookNames() []string {
	out := make([]string, 0, len(hookspec.Registry))
	for _, s := range hookspec.Registry {
		out = append(out, s.Name)
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
func configureSections() []string { return configure.Sections }

// modelPinSlots offers "slot=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinSlots() []string {
	out := make([]string, 0, len(models.Slots)+1)
	for _, s := range models.Slots {
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

// agentNames lists the `ask --agent` runner names; agent.Runners() only builds the roster, it never
// dials a runner.
func agentNames() []string {
	var out []string
	for _, r := range agent.Runners() {
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
	out = append(out, secrets.Names()...)
	slices.Sort(out)
	return slices.Compact(out)
}

// memoryRepoNames lists the catalog's repo keys (dirs under memory/ with a valid origin record).
func memoryRepoNames() []string {
	ents, err := memory.RepoDirs()
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

// dispatchCachePath is where `network tailscale --machines` leaves its last successful listing for completion
// to read; completion never calls the network itself.
func dispatchCachePath() string { return filepath.Join(paths.StateDir(), "tailscale-machines.json") }

// dispatchCacheTTL bounds how stale the cached machine list may be before completion ignores it.
const dispatchCacheTTL = 10 * time.Minute

type dispatchMachineCache struct {
	Schema int       `json:"schema"`
	At     time.Time `json:"at"`
	Names  []string  `json:"names"`
}

// writeDispatchCache is called by `network tailscale --machines` on a successful listing; best-effort, never
// surfaced as an error to the caller.
func writeDispatchCache(names []string) {
	data, err := json.Marshal(dispatchMachineCache{Schema: dispatchCacheSchema, At: time.Now(), Names: names})
	if err != nil {
		return
	}
	_ = os.MkdirAll(paths.StateDir(), 0o700)
	_ = os.WriteFile(dispatchCachePath(), data, 0o600)
}

// dispatchHostNames reads the cache `network tailscale --machines` last wrote, when it's fresh enough; nil
// (never an error, never a network call) otherwise — the Tab path has no time budget for ssh/API calls.
func dispatchHostNames() []string {
	data, err := os.ReadFile(dispatchCachePath())
	if err != nil {
		return nil
	}
	var c dispatchMachineCache
	if !decodeDispatchCache(data, &c) || time.Since(c.At) > dispatchCacheTTL {
		return nil
	}
	return c.Names
}

// dispatchAgentNames is dispatch.Agents, named to match the other *Names domain functions.
func dispatchAgentNames() []string { return dispatch.Agents }

// webViewAgentIDs lists subagent ids from the current project's most recent sessions — a bounded
// directory glob (no transcript parsing), current session first, fast enough for the Tab path.
func webViewAgentIDs() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	projectDir := filepath.Join(paths.ConfigDir(), "projects", memory.ProjectSlug(cwd))
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
