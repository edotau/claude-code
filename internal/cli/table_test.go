package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
)

func candidates(words ...string) []string { return completionEngine().Candidates(words) }

// candidatesWithin fails the test if completion hangs, which would mean it dispatched a run func.
func candidatesWithin(t *testing.T, words ...string) []string {
	t.Helper()
	done := make(chan []string, 1)
	go func() { done <- candidates(words...) }()
	select {
	case got := <-done:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("Candidates(%v) hung: completion must never dispatch a run func", words)
		return nil
	}
}

func sortedSubwords(n *helptext.Node) []string {
	var out []string
	for w := range n.Sub {
		out = append(out, w)
	}
	slices.Sort(out)
	return out
}

// vendorSubwords is "ask" plus every vendor subverb page, sorted.
func vendorSubwords() []string {
	out := []string{"ask"}
	for sub := range agentSubPages {
		out = append(out, sub)
	}
	slices.Sort(out)
	return out
}

func TestCompletionTopLevel(t *testing.T) {
	got := candidates("")
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

// TestCompletionExact: each word list completes to exactly the want list; nil means none (shell falls back to files).
func TestCompletionExact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []string
		want  []string
	}{
		{"prefix filter", []string{"me"}, []string{"memory"}},
		{"vault subcommand", []string{"vault", "s"}, []string{"set"}},
		{"memory subcommand", []string{"memory", "rev"}, []string{"review", "revise"}},
		{"leaf offers nothing", []string{"gateway", "route", "--task", ""}, nil},
		{"gemini ask despite bare launcher", []string{"gemini", "a"}, []string{"ask"}},
		{"install flags", []string{"install", "--"}, []string{"--dry-run", "--missing", "--setup", "--tools", "--verbose"}},
		{"theme flags", []string{"theme", "--"}, []string{"--palettes", "--reset"}},
		{"doctor flags", []string{"doctor", "--"}, []string{"--agent", "--audit", "--days", "--format", "--git", "--mcp",
			"--permissions", "--prompts", "--range", "--refresh", "--scan", "--secrets", "--since"}},
		{"switch values", []string{"switch", ""}, providers.ProfileNames()},
		{"ask --agent values", []string{"agents", "ask", "--agent", ""}, []string{"claude", "codex", "copilot", "gemini", "opencode"}},
		{"ask --model has no domain", []string{"agents", "ask", "--model", ""}, nil},
		{"workflow skeleton positional", []string{"workflow", "skeleton", ""}, workflowPatterns()},
		{"bare flag keeps prefix match", []string{"agents", "ask", "--agent"}, []string{"--agent", "--agents"}},
		{"vendor past double dash", []string{"codex", "--", ""}, nil},
		{"resolve-agent domain", []string{"worktree", "merge", "--resolve-agent", ""},
			[]string{"claude", "codex", "copilot", "gemini", "opencode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidates(tc.words...); !slices.Equal(got, tc.want) {
				t.Errorf("%v = %v, want %v", tc.words, got, tc.want)
			}
		})
	}
}

// TestCompletionContains: each word list offers at least the wanted candidates.
func TestCompletionContains(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []string
		want  []string
	}{
		{"gemini ask shares --agent", []string{"gemini", "ask", "--"}, []string{"--agent"}},
		{"completion --install", []string{"completion", "--"}, []string{"--install"}},
		{"usage --show", []string{"usage", "--"}, []string{"--show"}},
		{"preflight --live", []string{"preflight", "--"}, []string{"--live"}},
		{"configure --no-agents", []string{"configure", "--"}, []string{"--no-agents"}},
		{"databricks --profile", []string{"databricks", "--"}, []string{"--profile"}},
		{"clip --serve", []string{"clip", "--"}, []string{"--serve"}},
		{"theme values", []string{"theme", ""}, append([]string{"apply", "statusline", "vscode"}, theme.GradientNames()...)},
		{"network subverbs", []string{"network", ""}, []string{"tailscale", "browser", "websearch", "restart", "remote"}},
		{"--flag=value", []string{"agents", "ask", "--agent=co"}, []string{"--agent=codex"}},
		{"vendor status flag", []string{"codex", "status", "--"}, []string{"--all"}},
		{"vendor setup flag", []string{"codex", "setup", "--"}, []string{"--format"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := candidates(tc.words...)
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("%v = %v, missing %q", tc.words, got, want)
				}
			}
		})
	}
}

func TestCompletionAfterBoolFlagOffersNextFlag(t *testing.T) {
	for _, tc := range []struct {
		words []string
		typed string
	}{
		{[]string{"clean", "--dry-run", ""}, "--dry-run"},
		{[]string{"usage", "--workflows", ""}, "--workflows"},
		{[]string{"network", "remote", "--status", ""}, "--status"},
	} {
		got := candidates(tc.words...)
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

func TestCompletionSkipsTypedFlags(t *testing.T) {
	got := candidates("usage", "--workflows", "--")
	if slices.Contains(got, "--workflows") {
		t.Errorf("typed --workflows must drop out: %v", got)
	}
	for _, want := range []string{"--days", "--session", "--format"} {
		if !slices.Contains(got, want) {
			t.Errorf("usage --workflows -- candidates %v missing %q", got, want)
		}
	}
}

func TestFlagEqualsValueFormKeepsPrefix(t *testing.T) {
	got := candidates("agents", "ask", "--agent=co")
	for _, g := range got {
		if !strings.HasPrefix(g, "--agent=") {
			t.Errorf("ask --agent=co = %v, every candidate must keep the --agent= prefix", got)
		}
	}
}

// helpFlagRE pulls each long flag out of a FLAGS row label ("  -n, --name string  desc").
var helpFlagRE = regexp.MustCompile(`^  (?:-[A-Za-z0-9], )?--([A-Za-z0-9][A-Za-z0-9_-]*)`)

func TestCompletionMatchesHelpFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	vaulttest.UseFreshKey(t)
	t.Setenv("HOME", t.TempDir())
	tree, exempt := specTree(), helpFlagDriftExempt()
	for _, c := range commands() {
		if !c.sideEffectHelp {
			checkSpecMatchesHelp(t, []string{c.name}, tree[c.name], exempt)
		}
	}
}

// helpFlagDriftExempt maps spec paths whose help and completion differ to the reason.
func helpFlagDriftExempt() map[string]string {
	m := map[string]string{"anthropic science": "-h is forwarded to the external claude-science CLI"}
	for _, v := range []string{"copilot", "codex", "gemini", "opencode"} {
		m[v] = "bare launcher: its native flags never complete before a subword (noComplete)"
	}
	for _, n := range reviewToolNames() {
		m["review "+n] = "detectors keep their former scripts' flags; reviewSpec offers none on purpose"
	}
	for _, s := range []string{"pr list", "pr view", "pr diff", "pr create", "checks", "comment"} {
		m["scm "+s] = "help prints the shared scm flag set; completion offers each subverb's own"
	}
	return m
}

func checkSpecMatchesHelp(t *testing.T, path []string, node *helptext.Node, exempt map[string]string) {
	t.Helper()
	for word, sub := range node.Sub {
		checkSpecMatchesHelp(t, append(slices.Clone(path), word), sub, exempt)
	}
	label := strings.Join(path, " ")
	if exempt[label] != "" {
		return
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run(append(append([]string{"claude-code"}, path...), "-h"))
	})
	if code != 0 {
		t.Errorf("%s -h: exit %d\n%s", label, code, stdout+stderr)
		return
	}
	var spec []string
	for _, f := range node.Flags {
		if len(f.Name) > 1 { // help renders one-letter flags as -n, outside helpFlagRE
			spec = append(spec, f.Name)
		}
	}
	slices.Sort(spec)
	if help := helpLongFlags(stdout + stderr); !slices.Equal(help, spec) {
		t.Errorf("%s: -h documents %v, completion offers %v", label, help, spec)
	}
}

func helpLongFlags(text string) []string {
	var out []string
	inFlags := false
	for line := range strings.SplitSeq(text, "\n") {
		if line == "FLAGS" {
			inFlags = true
			continue
		}
		if line == "" || !strings.HasPrefix(line, "  ") {
			inFlags = false
		}
		if m := helpFlagRE.FindStringSubmatch(line); inFlags && m != nil && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// TestLaunchersNeverOfferFlagsOrDispatch: a bare launcher's own flags are never ours to complete.
func TestLaunchersNeverOfferFlagsOrDispatch(t *testing.T) {
	for _, name := range []string{"codex", "copilot", "opencode", "gemini", "claude"} {
		if lookup(name) == nil {
			t.Errorf("launcher %q is not a registered verb", name)
		}
		if n := specTree()[name]; n != nil && len(n.Flags) != 0 {
			t.Errorf("specTree()[%q] must not carry its own flags (bare launcher)", name)
		}
		if got := candidatesWithin(t, name, "--"); len(got) != 0 {
			t.Errorf("Candidates(%q --) = %v, want none", name, got)
		}
	}
}

func TestCompleteDispatch(t *testing.T) {
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdComplete([]string{"me"}) })
	if code != 0 {
		t.Fatalf("__complete me exit %d", code)
	}
	if strings.TrimSpace(out) != "memory" {
		t.Errorf("__complete me = %q, want memory", out)
	}
}

// TestCompletionScriptBash checks syntax, registers only claude-code, and drives completion against an argv stub.
func TestCompletionScriptBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "completion.bash")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"bash"}) })
	if code != 0 {
		t.Fatalf("completion bash exit %d", code)
	}
	if err := os.WriteFile(script, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	argvFile := filepath.Join(dir, "argv")
	stub := "#!" + bash + "\nprintf '%s\\n' \"$@\" > " + textutil.ShellQuote(argvFile) + "\necho codex\n"
	if err := os.WriteFile(filepath.Join(dir, "claude-code"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only words through COMP_CWORD reach the harness, excluding words after the cursor.
	drive := "source " + textutil.ShellQuote(script) + "\n" +
		`complete -p codex 2>/dev/null && exit 3; complete -p claude-code >&2` + "\n" +
		`COMP_WORDS=(claude-code codex d ignored); COMP_CWORD=2` + "\n" +
		`_claude_code` + "\n" +
		`printf '%s\n' "${COMPREPLY[@]}"`
	cmd := exec.Command(bash, "-c", drive)
	cmd.Env = append(cmd.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	outb, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash drive (exit 3 = codex registered): %v\n%s%s", err, outb, stderr.String())
	}
	if want := "complete -o default -F _claude_code claude-code"; strings.TrimSpace(stderr.String()) != want {
		t.Errorf("registration = %q, want %q", stderr.String(), want)
	}
	if strings.TrimSpace(string(outb)) != "codex" {
		t.Errorf("COMPREPLY = %q, want codex", outb)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(argv)); !slices.Equal(got, []string{"__complete", "codex", "d"}) {
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
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed script: %v", err)
	}
	if !strings.Contains(string(body), "_claude_code()") {
		t.Errorf("installed file is not the completion script:\n%s", body)
	}

	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"--install"}) }); code != 0 {
		t.Fatalf("completion --install (XDG fallback) exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(xdg, "bash-completion", "completions", "claude-code")); err != nil {
		t.Errorf("XDG_DATA_HOME fallback did not install: %v", err)
	}
}

func TestCompletionHelpExitsZeroWithoutInstalling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASH_COMPLETION_USER_DIR", dir)
	for _, flagArg := range []string{"-h", "--help"} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{flagArg}) })
		if code != 0 {
			t.Errorf("completion %s exit %d, want 0", flagArg, code)
		}
		if out := stdout + stderr; !strings.Contains(out, "usage: "+completionPage.Usage) {
			t.Errorf("completion %s missing usage line:\n%s", flagArg, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "completions", "claude-code")); err == nil {
		t.Error("-h/--help must not install anything")
	}
}

func TestCompletionUnknownArgExitsTwo(t *testing.T) {
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdCompletion([]string{"--bogus"}) }); code != 2 {
		t.Errorf("completion --bogus exit %d, want 2", code)
	}
}

// isolateCompletionEnv points every on-disk domain at a fresh temp dir so tests never read this machine's state.
func isolateCompletionEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
}

// TestGoldenProbesNowNonEmpty: every probe once recorded as empty now returns at least one candidate.
func TestGoldenProbesNowNonEmpty(t *testing.T) {
	isolateCompletionEnv(t)
	for _, tc := range []struct {
		name  string
		words []string
	}{
		{"switch openrouter", []string{"switch", "openrouter", ""}},
		{"agents sync --agent", []string{"agents", "sync", "--agent", ""}},
		{"ask --agent codex", []string{"agents", "ask", "--agent", "codex", ""}},
		{"vault set", []string{"vault", "set", ""}},
		{"models --pin", []string{"models", "--pin", ""}},
		{"configure", []string{"configure", ""}},
		{"network tailscale --exit-node", []string{"network", "tailscale", "--exit-node", ""}},
		{"memory alias", []string{"memory", "alias", ""}},
		{"ask --agent=", []string{"agents", "ask", "--agent="}},
	} {
		if got := candidates(tc.words...); len(got) == 0 {
			t.Errorf("%s = %v, want at least one candidate", tc.name, got)
		}
	}

	// memory.RepoKeys reads the catalog, so the repo store needs a seeded repo.
	repoDir := filepath.Join(memory.StoreDir(), "fixturerepo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := memory.WriteOrigin(repoDir, memory.Origin{Version: memory.OriginVersion, Key: "fixturerepo", Origin: "/fixture/origin"}); err != nil {
		t.Fatal(err)
	}
	if got := candidates("memory", "search", "--repo", ""); !slices.Contains(got, "fixturerepo") {
		t.Errorf("memory search --repo = %v, want it to contain the seeded repo %q", got, "fixturerepo")
	}

	// CachedMachineNames never calls the network, so a written cache is enough.
	tailscale.WriteMachinesCache([]string{"fixture-host"})
	if got := candidates("network", "remote", "--host", ""); !slices.Equal(got, []string{"fixture-host"}) {
		t.Errorf("dispatch --host = %v, want [fixture-host]", got)
	}
}

func TestCompletionNeverInvokesARunFunc(t *testing.T) {
	isolateCompletionEnv(t)
	for _, c := range commands() {
		candidatesWithin(t, c.name, "")
		for sub := range specTree()[c.name].Sub {
			candidatesWithin(t, c.name, sub, "")
		}
	}
}

// TestVendorCompletionOffersEverySubverb: `<vendor> <tab>` offers "ask" plus every vendor subverb page.
func TestVendorCompletionOffersEverySubverb(t *testing.T) {
	want := vendorSubwords()
	for _, vendor := range agent.Names() {
		got := candidates(vendor, "")
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("Candidates([%q \"\"]) = %v, want %v", vendor, got, want)
		}
	}
}

// TestSpecTreeSubwordsMatchDispatch: a word added to a verb's dispatch switch must get a specTree entry.
func TestSpecTreeSubwordsMatchDispatch(t *testing.T) {
	vaulttest.UseFreshKey(t)
	tree := specTree()
	for verb, want := range map[string][]string{
		"history": {"snapshot", "log", "diff"},
		"vault":   {"ls", "set", "get", "delete", "import", "export", "rotate-secret"},
		"network": {"tailscale", "browser", "websearch", "restart", "remote"},
		"memory": {"recall", "remember", "init", "review", "promote", "update", "dream", "search", "show",
			"digest", "index", "path", "catchup", "watch", "timeline", "alias", "import", "fmt", "report", "revise", "sandbox"},
		"gateway":  {"proxy"},
		"agents":   {"sync", "tree", "reap", "list", "ask", "dispatch", "label", "diff", "find", "send", "status", "result", "tail", "cancel", "wait", "brief"},
		"workflow": {"skeleton", "lint", "sync"},
		"theme":    {"apply", "statusline", "vscode"},
		"worktree": {"list", "create", "merge", "clean", "clean-all", "gate"},
		"gemini":   vendorSubwords(),
		"review":   reviewToolNames(),
	} {
		node := tree[verb]
		if node == nil {
			t.Errorf("%s: no node in specTree()", verb)
			continue
		}
		slices.Sort(want)
		if got := sortedSubwords(node); !slices.Equal(got, want) {
			t.Errorf("%s subwords = %v, want %v", verb, got, want)
		}
	}
	// install --setup <mode> is its own sub-dispatch, nested under install's "--setup" node.
	setupNode := tree["install"].Sub["--setup"]
	if setupNode == nil {
		t.Fatal(`install: no "--setup" sub-node in specTree()`)
	}
	if got, want := sortedSubwords(setupNode), []string{"adopt", "nuke", "workspace"}; !slices.Equal(got, want) {
		t.Errorf("install --setup subwords = %v, want %v", got, want)
	}
}

// TestHookDomainMatchesRegistry: `hook <tab>` offers every event settings.Specs declares, in registry order.
func TestHookDomainMatchesRegistry(t *testing.T) {
	var want []string
	for _, s := range settings.Specs() {
		if !slices.Contains(want, s.Event) {
			want = append(want, s.Event)
		}
	}
	if got := settings.HookEvents(); !slices.Equal(got, want) {
		t.Errorf("settings.HookEvents() = %v, want %v (settings.Registry)", got, want)
	}
	if got := candidates("hook", ""); !slices.Equal(got, want) {
		t.Errorf("hook = %v, want every registered event %v", got, want)
	}
}

// TestMemoryUpdateSpecHasNoRepoFlag: update takes no --repo, so the spec must not advertise one.
func TestMemoryUpdateSpecHasNoRepoFlag(t *testing.T) {
	node := specTree()["memory"].Sub["update"]
	if node == nil {
		t.Fatal("memory update: no sub-node in specTree()")
	}
	for _, f := range node.Flags {
		if f.Name == "repo" {
			t.Errorf("memory update: specTree() flags = %v, must not advertise --repo", node.Flags)
		}
	}
}

func TestRunDoesNotDispatchArgvZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"codex"}) })
	if code != 0 || stderr != "" {
		t.Fatalf("Run exit = %d, stderr = %q, want 0 and no error", code, stderr)
	}
	if want := captureUsage(t); stdout != want {
		t.Errorf("Run with argv[0]=codex output = %q, want usage %q", stdout, want)
	}
}

func TestRunHelpReportsWriteFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	closed, err := os.CreateTemp(t.TempDir(), "closed-stdout")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"", "help", "-h", "--help"} {
		t.Run("argument="+arg, func(t *testing.T) {
			argv := []string{"claude-code"}
			if arg != "" {
				argv = append(argv, arg)
			}
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				stdout := os.Stdout
				os.Stdout = closed
				defer func() { os.Stdout = stdout }()
				return Run(argv)
			})
			if code != 1 {
				t.Errorf("Run(%q) exit = %d, want 1 for failed help write", argv, code)
			}
			if !strings.Contains(stderr, "claude-code: write help:") || !strings.Contains(stderr, os.ErrClosed.Error()) {
				t.Errorf("stderr = %q, want help write diagnostic naming the closed file error", stderr)
			}
		})
	}
}

func TestHiddenClaudeVerbDispatchesButIsAbsentFromUsage(t *testing.T) {
	cliScratch(t)
	bin := pathstest.FakeVendor(t, "claude", "exit 99")
	c := lookup("claude")
	if c == nil || !c.hidden {
		t.Fatal("claude must be a registered hidden command")
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"claude-code", "claude", "--profile", "ultra", "--print-env"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("claude --print-env exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "binary   "+bin+"\n") {
		t.Errorf("claude --print-env did not resolve the vendor binary:\n%s", stdout)
	}
	code, stdout, stderr = hosttest.CaptureOutput(t, func() int { return Run([]string{"claude-code", "help"}) })
	if code != 0 || stderr != "" {
		t.Fatalf("help exit = %d, stderr = %q", code, stderr)
	}
	checkClaudeHiddenFromUsage(t, stdout)
}

func checkClaudeHiddenFromUsage(t *testing.T, stdout string) {
	t.Helper()
	for line := range strings.SplitSeq(stdout, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "claude" {
			t.Errorf("usage advertises hidden claude verb: %q", line)
		}
	}
}

// TestMenuRowsFit: every group is present, every verb name is unique and every row has a summary.
func TestMenuRowsFit(t *testing.T) {
	want := map[string]bool{groupAgents: false, groupAuth: false, groupSession: false, groupDiagnostics: false, groupSettings: false}
	names := map[string]bool{}
	for _, c := range commands() {
		if names[c.name] {
			t.Errorf("%s: declared twice", c.name)
		}
		names[c.name] = true
		if strings.TrimSpace(c.summary) == "" {
			t.Errorf("%s: empty summary", c.name)
		}
		if !c.hidden {
			if _, ok := want[c.group]; !ok {
				t.Errorf("%s: unknown group %q", c.name, c.group)
			}
			want[c.group] = true
		}
	}
	for g, seen := range want {
		if !seen {
			t.Errorf("group %s has no rows", g)
		}
	}
}

// Retired verbs (use was switch's old name) stay unknown: no dispatch, menu row or completion.
func TestOldVerbsAreUnknownCommands(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, old := range []string{"tailscale", "web", "mcp", "generate", "vscode", "settings", "statusline", "route",
		"router", "secrets", "providers", "token", "config", "use"} {
		if lookup(old) != nil {
			t.Errorf("lookup(%q) still resolves", old)
		}
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"claude-code", old}) })
		if code != 2 || !strings.Contains(stderr, fmt.Sprintf("unknown command %q", old)) {
			t.Errorf("%s: exit %d, stderr %q, want exit 2 unknown command", old, code, stderr)
		}
	}
	if lookup("switch") == nil {
		t.Error("switch (use's replacement) does not resolve")
	}
	for prefix, retired := range map[string]string{"us": "use", "tok": "token"} {
		if got := candidates(prefix); slices.Contains(got, retired) {
			t.Errorf("completing %q offers retired %q: %v", prefix, retired, got)
		}
	}
}

var helpFormatRow = regexp.MustCompile(`^  (\S(?:.*?\S)?) {2,}(\S.*)$`)
var helpFormatFlag = regexp.MustCompile(`^(?:-[A-Za-z0-9], )?(?:-[A-Za-z0-9]|--[A-Za-z0-9][A-Za-z0-9_-]+)(?: \S+)?$`)

func TestHelpFormat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	hidden := []string{
		"agents", "models", "review", "workflow", "hook",
		"help-svg", "clip", "completion", "plan", "test",
	}
	var verbs []string
	for _, c := range commands() {
		if !c.hidden {
			verbs = append(verbs, c.name)
		}
	}
	verbs = append(verbs, hidden...)
	for _, verb := range verbs {
		t.Run(verb, func(t *testing.T) {
			checkHelpFormat(t, []string{verb}, nil)
		})
	}
}

func TestTopLevelCommandRowsSingleWordSingleLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	vaulttest.UseFreshKey(t)
	t.Setenv("HOME", t.TempDir())
	for _, c := range commands() {
		t.Run(c.name, func(t *testing.T) {
			if c.sideEffectHelp {
				t.Skip("external CLI launcher or wl-paste proxy: -h is forwarded, not a help page")
			}
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return Run([]string{"claude-code", c.name, "-h"})
			})
			if code != 0 {
				t.Fatalf("%s -h: exit %d", c.name, code)
			}
			checkCommandRows(t, stdout+stderr)
		})
	}
}

// checkCommandRows: every COMMANDS label is one bare word and every row fits one line of <=80 columns.
func checkCommandRows(t *testing.T, text string) {
	t.Helper()
	inCommands := false
	label, row := "", ""
	for line := range strings.SplitSeq(text, "\n") {
		if line == "COMMANDS" {
			inCommands = true
			continue
		}
		if line == "" || !strings.HasPrefix(line, "  ") {
			inCommands = false
			continue
		}
		if !inCommands {
			continue
		}
		match := helpFormatRow.FindStringSubmatch(line)
		if strings.HasPrefix(line, "   ") {
			t.Errorf("label %q row %q has continuation %q", label, row, line)
			continue
		}
		if match == nil {
			t.Errorf("malformed COMMANDS row %q", line)
			continue
		}
		label, row = match[1], line
		checkCommandLabel(t, label, row)
	}
}

func checkCommandLabel(t *testing.T, label, row string) {
	t.Helper()
	if len(strings.Fields(label)) != 1 || strings.ContainsAny(label, "[<") {
		t.Errorf("COMMANDS label %q must be one bare word; row %q", label, row)
	}
	if width := termWidth(row); width > 80 {
		t.Errorf("COMMANDS label %q row %q has %d columns, want <=80", label, row, width)
	}
}

func checkHelpFormat(t *testing.T, path []string, parents []string) {
	t.Helper()
	argv := append([]string{"claude-code"}, path...)
	argv = append(argv, "-h")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(argv) })
	text := stdout + stderr
	if code != 0 {
		t.Errorf("%s: exit %d, want 0:\n%s", strings.Join(argv, " "), code, text)
	}
	checkCommandRows(t, text)
	checkHelpHeader(t, text)
	subs, rows := checkHelpRows(t, text)
	if rows == 0 && !helpAllowsNoRows(path) {
		t.Errorf("help has no rows under COMMANDS or FLAGS:\n%s", text)
	}
	// Parent-page help is intentional for some flagless actions; never dispatch its rows again.
	// `anthropic science` forwards -h to the external claude-science CLI.
	if slices.Contains(parents, text) || path[0] == "hook" || path[0] == "anthropic" {
		return
	}
	if len(path) >= 8 && len(subs) > 0 {
		t.Fatal("help command nesting exceeds eight levels")
	}
	for _, sub := range subs {
		t.Run(sub, func(t *testing.T) {
			checkHelpFormat(t, append(append([]string{}, path...), sub), append(parents, text))
		})
	}
}

func checkHelpHeader(t *testing.T, text string) {
	t.Helper()
	first := ""
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			first = line
			break
		}
	}
	if !strings.HasPrefix(first, "usage: claude-code") {
		t.Errorf("first non-empty line must start with usage: claude-code:\n%s", text)
	}
	if strings.Contains(text, "Usage of") {
		t.Errorf("legacy Usage of header:\n%s", text)
	}
}

type helpRowChecks struct {
	subs                    []string
	seenCommands, seenFlags map[string]bool
}

func checkHelpRows(t *testing.T, text string) ([]string, int) {
	t.Helper()
	checks := helpRowChecks{seenCommands: map[string]bool{}, seenFlags: map[string]bool{}}
	section, rows := "", 0
	for line := range strings.SplitSeq(text, "\n") {
		if n := termWidth(line); n > 80 {
			t.Errorf("line has %d columns (tabs expanded), want <=80: %q", n, line)
		}
		if line == "COMMANDS" || line == "FLAGS" {
			section = line
			continue
		}
		if line == "" || !strings.HasPrefix(line, "  ") {
			section = ""
			continue
		}
		if section == "" || strings.HasPrefix(line, "   ") {
			continue
		}
		match := helpFormatRow.FindStringSubmatch(line)
		if match == nil {
			t.Errorf("malformed %s row: %q", section, line)
			continue
		}
		rows++
		checks.checkRow(t, section, match[1])
	}
	return checks.subs, rows
}

func (checks *helpRowChecks) checkRow(t *testing.T, section, label string) {
	t.Helper()
	word := strings.Fields(label)[0]
	switch section {
	case "COMMANDS":
		if checks.seenCommands[word] {
			t.Errorf("duplicate command row %q", word)
			return
		}
		checks.seenCommands[word] = true
		checks.subs = append(checks.subs, word)
	case "FLAGS":
		if !helpFormatFlag.MatchString(label) {
			t.Errorf("malformed flag label: %q", label)
		}
		// Run does not expose its FlagSet; validate generated rows for shape and uniqueness.
		if checks.seenFlags[word] {
			t.Errorf("duplicate flag row %q", word)
		}
		checks.seenFlags[word] = true
	}
}

func helpAllowsNoRows(path []string) bool {
	flagless := map[string]bool{
		"hook": true, "clip": true, "history diff": true, "workflow lint": true, "workflow sync": true,
		"vault delete": true, "test ramdisk": true,
		"opencode catalog remove": true, "opencode catalog reset": true,
	}
	if len(path) == 2 && slices.Contains([]string{"copilot", "codex", "gemini", "opencode"}, path[0]) &&
		slices.Contains([]string{"cancel", "catalog", "models", "resume", "on", "off"}, path[1]) {
		return true
	}
	return flagless[strings.Join(path, " ")]
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test") // the moved batch/envelope tests reach the proxy
	agent.StubRefreshListingForTest()
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	gateway.HolderComm = func(int) string { return "claude-code" } // router-state tests record this test binary's pid as the proxy
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}

// cliScratch isolates CLAUDE_CONFIG_DIR, every credential env var, and PATH (no real copilot/gh/etc.
// binary reachable) so provider/model commands never touch real machine state.
func cliScratch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no external credential helpers or other CLIs
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", providers.EnvSessionProfile} {
		t.Setenv(k, "")
	}
}

func renderHelpPage(page helptext.Page) string {
	var out bytes.Buffer
	page.Fprint(&out)
	return out.String()
}

// termWidth is a line's display width with tabs expanded to 8-column stops.
func termWidth(l string) int {
	w := 0
	for _, r := range l {
		if r == '\t' {
			w += 8 - w%8
		} else {
			w++
		}
	}
	return w
}

func TestCompletionAgentsDispatchWaves(t *testing.T) {
	got := candidates("agents", "dispatch", "--")
	for _, name := range []string{"waves", "wave", "dry-run", "status", "fresh", "gate", "provider", "profile", "profiles"} {
		if !slices.Contains(got, "--"+name) {
			t.Errorf("dispatch completions lack --%s: %v", name, got)
		}
	}
	if slices.Contains(candidates("agents", ""), "waves") {
		t.Fatal("waves must be a dispatch flag, not a verb")
	}
}
