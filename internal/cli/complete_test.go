package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	"github.com/edotau/claude-code/internal/agent/agentcmd"
	"github.com/edotau/claude-code/internal/describe"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// TestCompletionTopLevel: an empty partial offers every visible command (+help), never a hidden one.
func TestCompletionTopLevel(t *testing.T) {
	got := completionCandidates([]string{""})
	for _, want := range []string{"memory", "doctor", "install", "help"} {
		if !slices.Contains(got, want) {
			t.Errorf("top-level candidates missing %q: %v", want, got)
		}
	}
	for _, hidden := range []string{"models", "agents", "review", "workflow",
		"completion", "__complete", "hook", "statusline", "version", "switch", "resolve", "claude"} {
		if slices.Contains(got, hidden) {
			t.Errorf("top-level candidates must not offer hidden %q: %v", hidden, got)
		}
	}
}

// TestCompletionPrefixFilter: candidates are narrowed by the partial word.
func TestCompletionPrefixFilter(t *testing.T) {
	if got := completionCandidates([]string{"me"}); !slices.Equal(got, []string{"memory"}) {
		t.Errorf("candidates for %q = %v, want [memory]", "me", got)
	}
}

// TestCompletionRemovedToken: the removed token verb is never offered.
func TestCompletionRemovedToken(t *testing.T) {
	if got := completionCandidates([]string{"tok"}); slices.Contains(got, "token") {
		t.Errorf("completion offers removed token: %v", got)
	}
}

// TestCompletionSubcommands: a hand-dispatched verb completes its subcommand words (order doesn't
// matter — they come off a map, not a table); a leaf with no args/sub/flags (gateway proxy) offers
// nothing, so the shell falls back to filenames.
func TestCompletionSubcommands(t *testing.T) {
	got := completionCandidates([]string{"vault", "s"})
	slices.Sort(got)
	if want := []string{"set"}; !slices.Equal(got, want) {
		t.Errorf("vault s* = %v, want %v", got, want)
	}
	if got := completionCandidates([]string{"memory", "rev"}); !slices.Equal(got, []string{"review"}) {
		t.Errorf("memory rev* = %v, want [review]", got)
	}
	if got := completionCandidates([]string{"gateway", "route", "--task", ""}); len(got) != 0 {
		t.Errorf("leaf command should offer no candidates, got %v", got)
	}
}

// TestGeminiSubverbsCompleteDespiteBareLauncherGuard: gemini is a bare launcher (noComplete), but its
// hand-dispatched ask subword are safe (in-process) and must still complete normally.
func TestGeminiSubverbsCompleteDespiteBareLauncherGuard(t *testing.T) {
	if got := completionCandidates([]string{"gemini", "a"}); !slices.Equal(got, []string{"ask"}) {
		t.Errorf("gemini a* = %v, want [ask]", got)
	}
	if got := completionCandidates([]string{"gemini", "ask", "--"}); !slices.Contains(got, "--agent") {
		t.Errorf("gemini ask flags = %v, missing --agent (shared with `ask`)", got)
	}
}

// TestCompletionFlags: a partial word beginning with "-" resolves the typed path and offers its own
// flags; a leaf with no flagsFor entry offers nothing.
func TestCompletionFlags(t *testing.T) {
	if got := completionCandidates([]string{"install", "--"}); !slices.Equal(got, []string{"--dry-run", "--setup", "--tools", "--verbose"}) {
		t.Errorf("install -- = %v, want [--dry-run --setup --tools --verbose]", got)
	}
	if got := completionCandidates([]string{"theme", "--"}); !slices.Equal(got, []string{"--palettes", "--reset"}) {
		t.Errorf("theme -- = %v, want [--palettes --reset]", got)
	}
	if got := completionCandidates([]string{"doctor", "--"}); !slices.Equal(got, []string{"--audit", "--days", "--format", "--git", "--mcp", "--prompts", "--range", "--refresh", "--scan", "--secrets", "--since"}) {
		t.Errorf("doctor -- = %v, want [--audit --days --format --git --mcp --prompts --range --refresh --scan --secrets --since]", got)
	}
}

// TestCompletionOffersHelpOnlyFlags: flags each verb's -h lists but the spec tree once omitted still complete.
func TestCompletionOffersHelpOnlyFlags(t *testing.T) {
	for verb, want := range map[string]string{"completion": "--install", "usage": "--show", "preflight": "--live",
		"configure": "--no-agents", "databricks": "--profile", "clip": "--serve"} {
		if got := completionCandidates([]string{verb, "--"}); !slices.Contains(got, want) {
			t.Errorf("%s -- = %v, want it to offer %s", verb, got, want)
		}
	}
}

// TestCompletionFlagValues: the word after a value flag completes from its domain; a flag with no
// registered domain (--model) offers nothing (filenames fallback), not an error.
func TestCompletionFlagValues(t *testing.T) {
	if got := completionCandidates([]string{"switch", ""}); !slices.Equal(got, providers.ProfileNames()) {
		t.Errorf("switch values = %v, want %v", got, providers.ProfileNames())
	}
	themeGot := completionCandidates([]string{"theme", ""})
	for _, want := range append([]string{"apply", "statusline", "vscode"}, theme.GradientNames()...) {
		if !slices.Contains(themeGot, want) {
			t.Errorf("theme values = %v, missing %q", themeGot, want)
		}
	}
	if got := completionCandidates([]string{"agents", "ask", "--agent", ""}); !slices.Equal(got, agentcmd.EngineNames()) {
		t.Errorf("ask --agent values = %v, want %v", got, agentcmd.EngineNames())
	}
	if got := completionCandidates([]string{"agents", "ask", "--model", ""}); len(got) != 0 {
		t.Errorf("ask --model values = %v, want none (no registered domain)", got)
	}
}

// TestCompletionPositionals: a verb's first bare argument completes from its own static domain.
func TestCompletionPositionals(t *testing.T) {
	if got := completionCandidates([]string{"hook", ""}); !slices.Equal(got, settings.HookEvents()) {
		t.Errorf("hook values = %v, want %v", got, settings.HookEvents())
	}
	if got := completionCandidates([]string{"workflow", "skeleton", ""}); !slices.Equal(got, workflowPatterns()) {
		t.Errorf("workflow skeleton values = %v, want %v", got, workflowPatterns())
	}
}

// TestCompletionAfterBoolFlagOffersNextFlag: the word after a boolean flag (no value slot) falls back
// to the remaining flags, across two different verbs.
func TestCompletionAfterBoolFlagOffersNextFlag(t *testing.T) {
	for _, tc := range []struct {
		words []string
		typed string
	}{
		{[]string{"clean", "--dry-run", ""}, "--dry-run"},
		{[]string{"usage", "--workflows", ""}, "--workflows"},
	} {
		got := completionCandidates(tc.words)
		if len(got) == 0 {
			t.Fatalf("%v offered no candidates; want the remaining flags", tc.words)
		}
		if slices.Contains(got, tc.typed) {
			t.Errorf("%v: already-typed %s must not be re-offered: %v", tc.words, tc.typed, got)
		}
		for _, g := range got {
			if g[0] != '-' {
				t.Errorf("%v: expected only flags after a boolean flag, got %v", tc.words, got)
			}
		}
	}
}

// TestCompletionSkipsTypedFlags: a flag already on the line is not offered again.
func TestCompletionSkipsTypedFlags(t *testing.T) {
	got := completionCandidates([]string{"usage", "--workflows", "--"})
	if slices.Contains(got, "--workflows") {
		t.Errorf("typed --workflows must drop out: %v", got)
	}
	for _, want := range []string{"--days", "--session", "--format"} {
		if !slices.Contains(got, want) {
			t.Errorf("usage --workflows -- candidates %v missing %q", got, want)
		}
	}
}

// TestLaunchersNeverOfferFlagsOrDispatch pins the hard rule in prose: a bare launcher verb's own
// flags belong to a process this binary never inspects, so completing them must offer nothing — and,
// since nothing here ever calls a command's run func, must never hang trying to launch one either.
func TestLaunchersNeverOfferFlagsOrDispatch(t *testing.T) {
	for _, name := range []string{"codex", "copilot", "opencode", "gemini", "claude"} {
		if n := specTree()[name]; n != nil && len(n.Flags) != 0 {
			t.Errorf("specTree()[%q] must not carry its own flags (bare launcher)", name)
		}
		done := make(chan []string, 1)
		go func() { done <- completionCandidates([]string{name, "--"}) }()
		select {
		case got := <-done:
			if len(got) != 0 {
				t.Errorf("completionCandidates(%q --) = %v, want none", name, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("completionCandidates(%q --) hung — completion must never dispatch a run func", name)
		}
	}
}

// TestCompletionLaunchersAreRealVerbs guards the expected launcher names against registry drift.
func TestCompletionLaunchersAreRealVerbs(t *testing.T) {
	for _, name := range []string{"codex", "copilot", "opencode", "gemini", "claude"} {
		if lookup(name) == nil {
			t.Errorf("noComplete lists %q, which is not a registered verb", name)
		}
	}
}

// TestCompleteDispatch: the hidden __complete verb prints one candidate per line.
func TestCompleteDispatch(t *testing.T) {
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdComplete([]string{"me"}) })
	if code != 0 {
		t.Fatalf("__complete me exit %d", code)
	}
	if strings.TrimSpace(out) != "memory" {
		t.Errorf("__complete me = %q, want memory", out)
	}
}

// TestCompletionScript: the emitted script registers only the harness binary.
func TestCompletionScript(t *testing.T) {
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"bash"}) })
	if code != 0 {
		t.Fatalf("completion bash exit %d", code)
	}
	for _, want := range []string{"complete -o default -F _claude_code claude-code", "__complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion script missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"_claude_code_verb", "codex", "gemini", "opencode", "copilot"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("completion script retained %q:\n%s", unwanted, out)
		}
	}
}

// TestCompletionScriptBash checks syntax and drives completion against a stub recording its argv.
func TestCompletionScriptBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "completion.bash")
	_, out, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"bash"}) })
	if err := writeFile(script, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	argvFile := filepath.Join(dir, "argv")
	stub := "#!" + bash + "\nprintf '%s\\n' \"$@\" > " + textutil.ShellQuote(argvFile) + "\necho codex\n"
	stubPath := filepath.Join(dir, "claude-code")
	if err := writeFile(stubPath, stub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only words through COMP_CWORD reach the harness, excluding words after the cursor.
	drive := "source " + textutil.ShellQuote(script) + "\n" +
		`COMP_WORDS=(claude-code codex d ignored); COMP_CWORD=2` + "\n" +
		`_claude_code` + "\n" +
		`printf '%s\n' "${COMPREPLY[@]}"`
	cmd := exec.Command(bash, "-c", drive)
	cmd.Env = append(cmd.Environ(), "PATH="+dir+":"+currentPath())
	outb, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash drive: %v\n%s", err, outb)
	}
	if strings.TrimSpace(string(outb)) != "codex" {
		t.Errorf("COMPREPLY = %q, want codex", outb)
	}
	argv, err := readFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(argv); !slices.Equal(got, []string{"__complete", "codex", "d"}) {
		t.Errorf("completion argv = %v, want [__complete codex d]", got)
	}
}

// TestCompletionInstallHonorsBashCompletionUserDirOverride pins BASH_COMPLETION_USER_DIR and the XDG fallback.
func TestCompletionInstallHonorsBashCompletionUserDirOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv("BASH_COMPLETION_USER_DIR", override)
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // must be ignored while the override is set
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"--install"}) })
	if code != 0 {
		t.Fatalf("completion --install exit %d", code)
	}
	path := filepath.Join(override, "completions", "claude-code")
	if !strings.Contains(out, path) {
		t.Errorf("completion --install printed %q, want it to name %s", out, path)
	}
	body, err := readFile(path)
	if err != nil {
		t.Fatalf("read installed script: %v", err)
	}
	if !strings.Contains(body, "_claude_code()") {
		t.Errorf("installed file is not the completion script:\n%s", body)
	}

	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"--install"}) }); code != 0 {
		t.Fatalf("completion --install (XDG fallback) exit %d", code)
	}
	if _, err := readFile(filepath.Join(xdg, "bash-completion", "completions", "claude-code")); err != nil {
		t.Errorf("XDG_DATA_HOME fallback did not install: %v", err)
	}
}

// TestCompletionHelpExitsZeroWithoutInstalling: -h/--help prints usage and returns 0 without writing.
func TestCompletionHelpExitsZeroWithoutInstalling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASH_COMPLETION_USER_DIR", dir)
	for _, flagArg := range []string{"-h", "--help"} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{flagArg}) })
		out := stdout + stderr
		if code != 0 {
			t.Errorf("completion %s exit %d, want 0", flagArg, code)
		}
		if !strings.Contains(out, "usage: "+completionPage.Usage) {
			t.Errorf("completion %s missing usage line:\n%s", flagArg, out)
		}
	}
	if _, err := readFile(filepath.Join(dir, "completions", "claude-code")); err == nil {
		t.Error("-h/--help must not install anything")
	}
}

// TestCompletionUnknownArgExitsTwo: an unrecognized argument is a usage error, not a silent no-op.
func TestCompletionUnknownArgExitsTwo(t *testing.T) {
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"--bogus"}) }); code != 2 {
		t.Errorf("completion --bogus exit %d, want 2", code)
	}
}

// writeFile is os.WriteFile with the test's own name, kept beside the completion tests that use it.
func writeFile(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}

// currentPath is the test process's PATH, so a stub dir can be prepended without losing bash's tools.
func currentPath() string { return os.Getenv("PATH") }

// readFile returns path's contents as a string.
func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// TestCompletionMemoryMigrateGone: the retired migrate verb no longer completes.
func TestCompletionMemoryMigrateGone(t *testing.T) {
	if got := completionCandidates([]string{"memory", "mi"}); slices.Contains(got, "migrate") {
		t.Errorf("memory mi* = %v, want no migrate", got)
	}
}

// isolateCompletionEnv points every on-disk domain (providers, secrets, memory repos, dispatch
// cache, worktree root) at a fresh temp dir, so a drift test never reads this machine's real state.
func isolateCompletionEnv(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	return dir
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
		{"ask --agent codex ⇥", []string{"agents", "ask", "--agent", "codex", ""}},
		{"vault set ⇥", []string{"vault", "set", ""}},
		{"models --pin ⇥", []string{"models", "--pin", ""}},
		{"configure ⇥", []string{"configure", ""}},
		{"network tailscale --exit-node ⇥", []string{"network", "tailscale", "--exit-node", ""}},
		{"memory archive ⇥", []string{"memory", "archive", ""}},
		{"ask --agent=⇥", []string{"agents", "ask", "--agent="}},
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

	// dispatch --host ⇥ needs a fresh cache — tailscale.CachedMachineNames() never calls the network itself.
	tailscale.WriteMachinesCache([]string{"fixture-host"})
	if got := completionCandidates([]string{"network", "remote", "--host", ""}); !slices.Equal(got, []string{"fixture-host"}) {
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

// TestNetworkBareOffersSubverbs pins `network ⇥`: all five sub-verbs.
func TestNetworkBareOffersSubverbs(t *testing.T) {
	got := completionCandidates([]string{"network", ""})
	for _, want := range []string{"tailscale", "browser", "websearch", "restart", "remote"} {
		if !slices.Contains(got, want) {
			t.Errorf("network ⇥ = %v, missing %q", got, want)
		}
	}
}

// TestDispatchStatusFlagThenRemainingFlags: the word after the boolean --status falls back to
// dispatch's remaining flags, with --status itself dropped.
func TestDispatchStatusFlagThenRemainingFlags(t *testing.T) {
	got := completionCandidates([]string{"network", "remote", "--status", ""})
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
	got := completionCandidates([]string{"agents", "ask", "--agent=co"})
	if !slices.Contains(got, "--agent=codex") {
		t.Errorf("ask --agent=co = %v, want it to contain --agent=codex", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "--agent=") {
			t.Errorf("ask --agent=co = %v, every candidate must keep the --agent= prefix", got)
		}
	}
	// A bare flag with no "=" is untouched by the new branch.
	if got := completionCandidates([]string{"agents", "ask", "--agent"}); !slices.Equal(got, []string{"--agent", "--agents"}) {
		t.Errorf("ask --agent (no =) = %v, want [--agent --agents] (prefix match, no = branch)", got)
	}
}

// TestNoCompleteLauncherStillOffersNothing: a bare launcher verb (noComplete) must offer nothing of
// its own beyond any in-process subwords — its flags belong to a process this binary never
// inspects, and nothing here may invoke a command's run func to find out what they are.
func TestNoCompleteLauncherStillOffersNothing(t *testing.T) {
	for _, name := range []string{"codex", "copilot", "opencode", "gemini", "claude"} {
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
		for sub := range specTree()[c.name].Sub {
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

// TestVendorCompletionOffersEverySubverb: tab-completing a vendor's own bare position (`codex ⇥`)
// offers "ask" plus every word in vendorSubverbs, for every vendor — derived off vendorSubverbs so a
// future addition (e.g. explore) is picked up without a second hand-copied list to keep in lockstep.
func TestVendorCompletionOffersEverySubverb(t *testing.T) {
	want := []string{"ask"}
	for sub := range agentcmd.Subverbs {
		want = append(want, sub)
	}
	slices.Sort(want)
	for _, vendor := range agentcmd.VendorNames() {
		got := completionCandidates([]string{vendor, ""})
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("completionCandidates([%q \"\"]) = %v, want %v", vendor, got, want)
		}
	}
}

// TestVendorCompletionSubverbFlags: a vendor subverb's own flags complete past it, pinning two
// representative cases (status's --all and setup's --format) against their helptext.Flag wiring.
func TestVendorCompletionSubverbFlags(t *testing.T) {
	if got := completionCandidates([]string{"codex", "status", "--"}); !slices.Contains(got, "--all") {
		t.Errorf("codex status -- = %v, missing --all", got)
	}
	if got := completionCandidates([]string{"codex", "setup", "--"}); !slices.Contains(got, "--format") {
		t.Errorf("codex setup -- = %v, missing --format", got)
	}
}

// TestVendorCompletionDoesNotFollowDoubleDash: `<v> -- ⇥` is past the vendor's own (unknown) native
// flags — it must offer nothing, not the subverb domain again (mirrors the old `run codex --` guard).
func TestVendorCompletionDoesNotFollowDoubleDash(t *testing.T) {
	if got := completionCandidates([]string{"codex", "--", ""}); len(got) != 0 {
		t.Errorf("codex -- ⇥ = %v, want none (codex's own flags are not ours to know)", got)
	}
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
	vaulttest.UseFreshKey(t)
	cases := map[string][]string{
		"history": {"snapshot", "log", "diff"},
		"vault":   {"ls", "set", "get", "delete", "import", "export", "edit", "rotate-secret"},
		"network": {"tailscale", "browser", "websearch", "restart", "remote"},
		"memory": {"recall", "remember", "init", "review", "verify", "promote", "update", "dream", "search", "show",
			"digest", "index", "path", "catchup", "watch", "timeline", "archive", "alias", "fmt", "report"},
		"gateway":  {"proxy"},
		"agents":   {"sync", "tree", "reap", "list", "ask", "dispatch", "label"},
		"workflow": {"skeleton", "lint"},
		"theme":    {"apply", "statusline", "vscode"},
		"worktree": {"list", "create", "merge", "clean", "clean-all", "gate"},
	}
	// gemini's subwords are "ask" (handled by vendorVerb's own if, not vendorSubverbs) plus every
	// vendor subverb — derived from vendorSubverbs, not hand-copied, so a future subverb never drifts.
	geminiWant := []string{"ask"}
	for sub := range agentcmd.Subverbs {
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
		for w := range node.Sub {
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

func checkInstallSetupSpec(t *testing.T, tree map[string]*helptext.Node) {
	t.Helper()
	// install --setup <mode> is its own sub-dispatch, nested under install's "--setup" node.
	installNode := tree["install"]
	if installNode == nil {
		t.Fatal("install: no node in specTree()")
	}
	setupNode := installNode.Sub["--setup"]
	if setupNode == nil {
		t.Fatal(`install: no "--setup" sub-node in specTree()`)
	}
	var gotSetup []string
	for w := range setupNode.Sub {
		gotSetup = append(gotSetup, w)
	}
	wantSetup := []string{"adopt", "nuke", "workspace"}
	slices.Sort(gotSetup)
	slices.Sort(wantSetup)
	if !slices.Equal(gotSetup, wantSetup) {
		t.Errorf("install --setup subwords = %v, want %v", gotSetup, wantSetup)
	}
}

func checkReviewSubwordSpec(t *testing.T, tree map[string]*helptext.Node) {
	t.Helper()
	// review derives its subwords from reviewTools itself (same source its dispatch switch reads),
	// so there is no separate hand list to drift — assert the derivation is wired, not duplicate it.
	reviewNode := tree["review"]
	if reviewNode == nil {
		t.Fatal("review: no node in specTree()")
	}
	var got []string
	for w := range reviewNode.Sub {
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
	got := settings.HookEvents()
	if !slices.Equal(got, want) {
		t.Errorf("settings.HookEvents() = %v, want %v (settings.Registry)", got, want)
	}
	if got := completionCandidates([]string{"hook", ""}); !slices.Equal(got, want) {
		t.Errorf("hook ⇥ = %v, want every registered event %v", got, want)
	}
}

// TestWorktreeMergeFlagsMatchItsOwnFlagSet pins worktree merge's completable flags against the
// literal flag sets in the merge verb (internal/worktree/worktreecmd/worktree.go) — catching exactly
// the kind of drift Part 3 replaced three hand tables to prevent.
func TestWorktreeMergeFlagsMatchItsOwnFlagSet(t *testing.T) {
	node := specTree()["worktree"].Sub["merge"]
	if node == nil {
		t.Fatal(`specTree()["worktree"].Sub["merge"] is nil`)
	}
	var got []string
	for _, f := range node.Flags {
		got = append(got, f.Name)
	}
	slices.Sort(got)
	want := []string{"dry-run", "full-verify", "no-clean", "no-verify", "resolve-agent", "resume", "target", "verify"}
	if !slices.Equal(got, want) {
		t.Errorf("worktree merge flags = %v, want %v (cmdWorktreeMerge's own flag.NewFlagSet)", got, want)
	}
}

// TestGenerateProfileDoctorModelsFlagsMatchTheirOwnFlagSets pins four more verbs' completable flags
// against the literal helptext.NewFlagSet calls in their own handlers (internal/configure/configurecmd/configure.go, internal/doctor/doctor.go,
// providers.go) — every flag the handler defines must have a specTree candidate, or
// tab-completion silently falls behind the handler the way askFlags once did.
func TestConfigureDoctorModelsFlagsMatchTheirOwnFlagSets(t *testing.T) {
	cases := map[string][]string{
		"configure": {"restore", "revert", "out", "agents", "agent", "profile", "dry-run", "check", "list", "format"}, // configurecmd.Run
		"doctor":    {"audit", "days", "format"},                                                                      // doctor.Run
		"models":    {"profile", "unpin", "live", "dry-run", "format", "pin"},                                         // cmdModels
	}
	for verb, flags := range cases {
		node := specTree()[verb]
		if node == nil {
			t.Fatalf("%s: no node in specTree()", verb)
		}
		got := describe.FlagNames(node)
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
	node := memNode.Sub["update"]
	if node == nil {
		t.Fatal("memory update: no sub-node in specTree()")
	}
	if got := describe.FlagNames(node); slices.Contains(got, "--repo") || slices.Contains(got, "--repo <value>") {
		t.Errorf("memory update: specTree() flags = %v, must not advertise --repo", got)
	}
}

// TestAskFlagsMatchItsOwnFlagSet pins ask's completable flags against the literal flag.NewFlagSet
// calls in Run (internal/agent/ask/ask.go) — askFlags had silently fallen behind by five flags
// (fallback, attach, keep-env, autonomous, enable-memory) before this test existed; background and
// job joined later (job dispatch control: --background detaches, --job replays a saved record).
func TestAskFlagsMatchItsOwnFlagSet(t *testing.T) {
	node := specTree()["agents"].Sub["ask"]
	if node == nil {
		t.Fatal(`specTree()["agents"].Sub["ask"] is nil`)
	}
	var got []string
	for _, f := range node.Flags {
		got = append(got, f.Name)
	}
	slices.Sort(got)
	want := []string{
		"agent", "agents", "allow", "ask", "as", "attach", "autonomous", "background", "batch", "debug-file", "deny", "effort", "enable-memory", "fail-fast", "fallback", "fleet",
		"format", "job", "jobs", "keep-env", "log", "mode", "model", "native", "no-persist", "no-tools", "overlay", "profile", "read-only", "retry", "rubric", "safe-mode", "session", "timeout",
		"tools", "workdir",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ask flags = %v, want %v (cmdAsk's own flag.NewFlagSet)", got, want)
	}
}
