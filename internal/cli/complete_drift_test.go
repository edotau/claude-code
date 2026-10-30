package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/memory"
)

// isolateCompletionEnv points every on-disk domain (providers, secrets, memory repos, dispatch
// cache, worktree root) at a fresh temp dir, so a drift test never reads this machine's real state.
func isolateCompletionEnv(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	return dir
}

// TestSpecTreeCoversEveryCommand: every verb commands() lists gets a node — a launcher is never
// silently unrepresented, even though it carries no flags/args of its own.
func TestSpecTreeCoversEveryCommand(t *testing.T) {
	tree := specTree()
	for _, c := range commands() {
		if tree[c.name] == nil {
			t.Errorf("specTree() has no node for verb %q", c.name)
		}
	}
}

// TestSpecTreeSubwordsMatchDispatch is table-driven against each verb's own hand-dispatch switch
// (grepped from the verb's run func at the time this test was written) — a word added to the
// switch without a matching specTree entry drifts silently otherwise.
func TestSpecTreeSubwordsMatchDispatch(t *testing.T) {
	useVaultTest(t)
	cases := map[string][]string{
		"history": {"snapshot", "log", "diff"},
		"vault":   {"list", "set", "get", "delete", "import", "export", "edit", "rotate-secret"},
		"network": {"tailscale", "browser", "websearch", "mcp", "page", "view", "vscode", "restart"},
		"memory": {"recall", "remember", "init", "review", "promote", "update", "dream", "search", "show",
			"digest", "index", "path", "watch", "timeline", "archive", "alias", "fmt"},
		"gateway":  {"router", "proxy"},
		"agents":   {"sync", "tree", "reap"},
		"workflow": {"skeleton", "lint"},
		"theme":    {"apply", "statusline", "vscode"},
		"worktree": {"list", "create", "merge", "clean", "clean-all"},
	}
	// gemini's subwords are "ask" (handled by vendorVerb's own if, not vendorSubverbs) plus every
	// vendor subverb plus its one extra, bridge — derived from vendorSubverbs, not hand-copied, so a
	// future subverb (e.g. explore) never drifts this test silently.
	geminiWant := []string{"ask", "bridge"}
	for sub := range vendorSubverbs {
		geminiWant = append(geminiWant, sub)
	}
	cases["gemini"] = geminiWant
	tree := specTree()
	for verb, want := range cases {
		node := tree[verb]
		if node == nil {
			t.Errorf("%s: no node in specTree()", verb)
			continue
		}
		var got []string
		for w := range node.sub {
			got = append(got, w)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s subwords = %v, want %v", verb, got, want)
		}
	}
	// install --setup <mode> is its own sub-dispatch, nested under install's "--setup" node.
	installNode := tree["install"]
	if installNode == nil {
		t.Fatal("install: no node in specTree()")
	}
	setupNode := installNode.sub["--setup"]
	if setupNode == nil {
		t.Fatal(`install: no "--setup" sub-node in specTree()`)
	}
	var gotSetup []string
	for w := range setupNode.sub {
		gotSetup = append(gotSetup, w)
	}
	wantSetup := []string{"adopt", "nuke", "workspace"}
	slices.Sort(gotSetup)
	slices.Sort(wantSetup)
	if !slices.Equal(gotSetup, wantSetup) {
		t.Errorf("install --setup subwords = %v, want %v", gotSetup, wantSetup)
	}
	// review derives its subwords from reviewTools itself (same source its dispatch switch reads),
	// so there is no separate hand list to drift — assert the derivation is wired, not duplicate it.
	reviewNode := tree["review"]
	if reviewNode == nil {
		t.Fatal("review: no node in specTree()")
	}
	var got []string
	for w := range reviewNode.sub {
		got = append(got, w)
	}
	slices.Sort(got)
	want := reviewToolNames()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("review subwords = %v, want %v (reviewToolNames)", got, want)
	}
}

// TestHookDomainMatchesRegistry: `hook ⇥` must offer every event hookspec.Registry declares, in
// registry order — the stale-table bug Part 3 fixed (7 hard-coded names vs. 21 registered).
func TestHookDomainMatchesRegistry(t *testing.T) {
	var want []string
	for _, s := range hookspec.Specs() {
		if !slices.Contains(want, s.Event) {
			want = append(want, s.Event)
		}
	}
	got := hookEvents()
	if !slices.Equal(got, want) {
		t.Errorf("hookEvents() = %v, want %v (hookspec.Registry)", got, want)
	}
	if got := completionCandidates([]string{"hook", ""}); !slices.Equal(got, want) {
		t.Errorf("hook ⇥ = %v, want every registered event %v", got, want)
	}
}

// TestGoldenProbesNowNonEmpty pins every probe Part 3's Facts section recorded as empty before the
// fix — each must now return at least one candidate.
func TestGoldenProbesNowNonEmpty(t *testing.T) {
	isolateCompletionEnv(t)
	for _, tc := range []struct {
		name  string
		words []string
	}{
		{"switch openrouter ⇥", []string{"switch", "openrouter", ""}},
		{"agents sync --agent ⇥", []string{"agents", "sync", "--agent", ""}},
		{"ask --agent codex ⇥", []string{"ask", "--agent", "codex", ""}},
		{"vault set ⇥", []string{"vault", "set", ""}},
		{"models --pin ⇥", []string{"models", "--pin", ""}},
		{"configure ⇥", []string{"configure", ""}},
		{"network tailscale --exit-node ⇥", []string{"network", "tailscale", "--exit-node", ""}},
		{"memory archive ⇥", []string{"memory", "archive", ""}},
		{"ask --agent=⇥", []string{"ask", "--agent="}},
	} {
		if got := completionCandidates(tc.words); len(got) == 0 {
			t.Errorf("%s = %v, want at least one candidate", tc.name, got)
		}
	}

	// memory search --repo ⇥ needs a seeded repo store — memoryRepoNames() reads the catalog, and an
	// empty fixture dir legitimately has none yet.
	storeDir := memory.StoreDir()
	repoDir := filepath.Join(storeDir, "fixturerepo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := memory.WriteOrigin(repoDir, memory.Origin{Version: memory.OriginVersion, Key: "fixturerepo", Origin: "/fixture/origin"}); err != nil {
		t.Fatal(err)
	}
	if got := completionCandidates([]string{"memory", "search", "--repo", ""}); !slices.Contains(got, "fixturerepo") {
		t.Errorf("memory search --repo ⇥ = %v, want it to contain the seeded repo %q", got, "fixturerepo")
	}

	// dispatch --host ⇥ needs a fresh cache — dispatchHostNames() never calls the network itself.
	writeDispatchCache([]string{"fixture-host"})
	if got := completionCandidates([]string{"remote", "--host", ""}); !slices.Equal(got, []string{"fixture-host"}) {
		t.Errorf("dispatch --host ⇥ = %v, want [fixture-host]", got)
	}
}

// TestWorktreeMergeResolveAgentCompletes pins the specific probe named in the task: the resolve-agent
// value domain is claude|codex, never a positional or sub-word collision.
func TestWorktreeMergeResolveAgentCompletes(t *testing.T) {
	got := completionCandidates([]string{"worktree", "merge", "--resolve-agent", ""})
	if !slices.Equal(got, []string{"claude", "codex"}) {
		t.Errorf("worktree merge --resolve-agent ⇥ = %v, want [claude codex]", got)
	}
}

// TestNetworkBareOffersSubverbs pins `network ⇥`: all six sub-verbs; the page flags live on `network page`.
func TestNetworkBareOffersSubverbs(t *testing.T) {
	got := completionCandidates([]string{"network", ""})
	for _, want := range []string{"tailscale", "browser", "websearch", "mcp", "page", "view", "vscode", "restart"} {
		if !slices.Contains(got, want) {
			t.Errorf("network ⇥ = %v, missing %q", got, want)
		}
	}
	page := completionCandidates([]string{"network", "page", ""})
	for _, want := range []string{"--addr", "--hosts"} {
		if !slices.Contains(page, want) {
			t.Errorf("network page ⇥ = %v, missing %q", page, want)
		}
	}
}

// TestDispatchStatusFlagThenRemainingFlags: the word after the boolean --status falls back to
// dispatch's remaining flags, with --status itself dropped.
func TestDispatchStatusFlagThenRemainingFlags(t *testing.T) {
	got := completionCandidates([]string{"remote", "--status", ""})
	if len(got) == 0 {
		t.Fatal("dispatch --status ⇥ offered nothing, want the remaining flags")
	}
	if slices.Contains(got, "--status") {
		t.Errorf("dispatch --status ⇥ = %v, must not re-offer --status", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "-") {
			t.Errorf("dispatch --status ⇥ = %v, want only flags", got)
		}
	}
}

// TestFlagEqualsValueForm: "--flag=value" completes the value and returns the whole "--flag=<cand>"
// token, the syntax Part 3 added (Facts: "no --flag=value form").
func TestFlagEqualsValueForm(t *testing.T) {
	got := completionCandidates([]string{"ask", "--agent=co"})
	if !slices.Contains(got, "--agent=codex") {
		t.Errorf("ask --agent=co = %v, want it to contain --agent=codex", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "--agent=") {
			t.Errorf("ask --agent=co = %v, every candidate must keep the --agent= prefix", got)
		}
	}
	// A bare flag with no "=" is untouched by the new branch.
	if got := completionCandidates([]string{"ask", "--agent"}); !slices.Equal(got, []string{"--agent"}) {
		t.Errorf("ask --agent (no =) = %v, want [--agent] unaffected", got)
	}
}

// TestNoCompleteLauncherStillOffersNothing: a bare launcher verb (noComplete) must offer nothing of
// its own beyond any in-process subwords — its flags belong to a process this binary never
// inspects, and nothing here may invoke a command's run func to find out what they are.
func TestNoCompleteLauncherStillOffersNothing(t *testing.T) {
	for name := range noComplete {
		if got := completionCandidates([]string{name, "--"}); len(got) != 0 {
			t.Errorf("%s -- = %v, want none (noComplete launcher)", name, got)
		}
	}
}

// TestCompletionNeverInvokesARunFunc is the construction proof behind every test in this file: build
// a specTree whose run funcs are poisoned sentinels, and drive every verb+subword path through
// completionCandidates — a hang or panic means something dereferenced a command's run func instead
// of the static spec tree.
func TestCompletionNeverInvokesARunFunc(t *testing.T) {
	isolateCompletionEnv(t)
	for _, c := range commands() {
		words := []string{c.name, ""}
		done := make(chan []string, 1)
		go func() { done <- completionCandidates(words) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("completionCandidates(%v) hung — must never call a run func", words)
		}
		for sub := range specTree()[c.name].sub {
			words := []string{c.name, sub, ""}
			done := make(chan []string, 1)
			go func() { done <- completionCandidates(words) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("completionCandidates(%v) hung — must never call a run func", words)
			}
		}
	}
}

// TestWorktreeMergeFlagsMatchItsOwnFlagSet pins worktree merge's completable flags against the
// literal flag.NewFlagSet calls in cmdWorktreeMerge (internal/cli/worktree.go) — catching exactly
// the kind of drift Part 3 replaced three hand tables to prevent.
func TestWorktreeMergeFlagsMatchItsOwnFlagSet(t *testing.T) {
	node := specTree()["worktree"].sub["merge"]
	if node == nil {
		t.Fatal(`specTree()["worktree"].sub["merge"] is nil`)
	}
	var got []string
	for _, f := range node.flags {
		got = append(got, f.name)
	}
	slices.Sort(got)
	want := []string{"dry-run", "full-verify", "no-clean", "no-verify", "resolve-agent", "resume", "target", "verify"}
	if !slices.Equal(got, want) {
		t.Errorf("worktree merge flags = %v, want %v (cmdWorktreeMerge's own flag.NewFlagSet)", got, want)
	}
}

// TestGenerateProfileDoctorModelsFlagsMatchTheirOwnFlagSets pins four more verbs' completable flags
// against the literal flag.NewFlagSet calls in their own handlers (internal/cli/launch.go,
// internal/cli/providers.go) — every flag the handler defines must have a specTree candidate, or
// tab-completion silently falls behind the handler the way askFlags once did.
func TestConfigureDoctorModelsFlagsMatchTheirOwnFlagSets(t *testing.T) {
	cases := map[string][]string{
		"configure": {"restore", "revert", "out", "agents", "agent", "profile", "dry-run", "check", "list", "format"}, // cmdConfigure
		"doctor":    {"audit", "days", "format"},                                                                      // cmdDoctor
		"models":    {"profile", "unpin", "live", "dry-run", "format", "pin"},                                         // cmdModels
	}
	for verb, flags := range cases {
		node := specTree()[verb]
		if node == nil {
			t.Fatalf("%s: no node in specTree()", verb)
		}
		got := flagNames(node)
		for _, f := range flags {
			boolForm, valuedForm := "--"+f, "--"+f+" <value>"
			if !slices.Contains(got, boolForm) && !slices.Contains(got, valuedForm) {
				t.Errorf("%s: specTree() flags = %v, missing --%s (from its own flag.NewFlagSet)", verb, got, f)
			}
		}
	}
}

// TestMemoryUpdateSpecHasNoRepoFlag: update takes no --repo in its own flag.FlagSet (memoryFlagSet,
// not memoryRepoFlagSet) — the spec must not advertise one either.
func TestMemoryUpdateSpecHasNoRepoFlag(t *testing.T) {
	memNode := specTree()["memory"]
	if memNode == nil {
		t.Fatal("memory: no node in specTree()")
	}
	node := memNode.sub["update"]
	if node == nil {
		t.Fatal("memory update: no sub-node in specTree()")
	}
	if got := flagNames(node); slices.Contains(got, "--repo") || slices.Contains(got, "--repo <value>") {
		t.Errorf("memory update: specTree() flags = %v, must not advertise --repo", got)
	}
}

// TestAskFlagsMatchItsOwnFlagSet pins ask's completable flags against the literal flag.NewFlagSet
// calls in cmdAsk (internal/cli/agent.go) — askFlags had silently fallen behind by five flags
// (fallback, attach, keep-env, autonomous, enable-memory) before this test existed; background and
// job joined later (job dispatch control: --background detaches, --job replays a saved record).
func TestAskFlagsMatchItsOwnFlagSet(t *testing.T) {
	node := specTree()["ask"]
	if node == nil {
		t.Fatal(`specTree()["ask"] is nil`)
	}
	var got []string
	for _, f := range node.flags {
		got = append(got, f.name)
	}
	slices.Sort(got)
	want := []string{
		"agent", "attach", "autonomous", "background", "batch", "debug-file", "effort", "enable-memory", "fail-fast", "fallback",
		"format", "job", "jobs", "keep-env", "log", "model", "native", "no-persist", "no-tools", "profile", "retry", "rubric", "safe-mode", "session", "timeout",
		"tools", "workdir",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ask flags = %v, want %v (cmdAsk's own flag.NewFlagSet)", got, want)
	}
}
