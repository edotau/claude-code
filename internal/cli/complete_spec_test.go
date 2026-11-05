package cli

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
)

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
		"network": {"tailscale", "browser", "websearch", "mcp", "page", "view", "vscode", "restart", "remote"},
		"memory": {"recall", "remember", "init", "review", "promote", "update", "dream", "search", "show",
			"digest", "index", "path", "catchup", "watch", "timeline", "archive", "alias", "fmt"},
		"gateway":  {"router", "proxy", "token"},
		"agents":   {"sync", "tree", "reap"},
		"workflow": {"skeleton", "lint"},
		"theme":    {"apply", "statusline", "vscode"},
		"worktree": {"list", "create", "merge", "clean", "clean-all"},
	}
	// gemini's subwords are "ask" (handled by vendorVerb's own if, not vendorSubverbs) plus every
	// vendor subverb plus its one extra, bridge — derived from vendorSubverbs, not hand-copied, so a
	// future subverb (e.g. explore) never drifts this test silently.
	geminiWant := []string{"ask", "bridge"}
	for sub := range agentSubverbs {
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
	checkInstallSetupSpec(t, tree)
	checkReviewSubwordSpec(t, tree)
}

func checkInstallSetupSpec(t *testing.T, tree map[string]*compNode) {
	t.Helper()
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
}

func checkReviewSubwordSpec(t *testing.T, tree map[string]*compNode) {
	t.Helper()
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

// TestHookDomainMatchesRegistry: `hook ⇥` must offer every event settings.Registry declares, in
// registry order — the stale-table bug Part 3 fixed (7 hard-coded names vs. 21 registered).
func TestHookDomainMatchesRegistry(t *testing.T) {
	var want []string
	for _, s := range settings.Specs() {
		if !slices.Contains(want, s.Event) {
			want = append(want, s.Event)
		}
	}
	got := hookEvents()
	if !slices.Equal(got, want) {
		t.Errorf("hookEvents() = %v, want %v (settings.Registry)", got, want)
	}
	if got := completionCandidates([]string{"hook", ""}); !slices.Equal(got, want) {
		t.Errorf("hook ⇥ = %v, want every registered event %v", got, want)
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
// calls in RunAsk (internal/cli/agents/ask.go) — askFlags had silently fallen behind by five flags
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
		"agent", "agents", "as", "attach", "autonomous", "background", "batch", "debug-file", "effort", "enable-memory", "fail-fast", "fallback", "fleet",
		"format", "job", "jobs", "keep-env", "log", "model", "native", "no-persist", "no-tools", "overlay", "profile", "read-only", "retry", "rubric", "safe-mode", "session", "timeout",
		"tools", "workdir",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ask flags = %v, want %v (cmdAsk's own flag.NewFlagSet)", got, want)
	}
}
