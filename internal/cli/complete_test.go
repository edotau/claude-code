package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/statusline"
)

// TestCompletionTopLevel: an empty partial offers every visible command (+help), never a hidden one.
func TestCompletionTopLevel(t *testing.T) {
	got := completionCandidates([]string{""})
	for _, want := range []string{"memory", "doctor", "install", "help"} {
		if !slices.Contains(got, want) {
			t.Errorf("top-level candidates missing %q: %v", want, got)
		}
	}
	for _, hidden := range []string{"models", "router", "ask", "agents", "review", "workflow",
		"completion", "__complete", "hook", "statusline", "version", "run", "switch", "resolve", "claude"} {
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

// TestCompletionAliasPrefix: an alias (token→resolve, use→switch) completes once a prefix is typed
// but stays off the bare double-Tab list, matching the canonical spelling advertised on the menu.
func TestCompletionAliasPrefix(t *testing.T) {
	if got := completionCandidates([]string{"tok"}); !slices.Equal(got, []string{"token"}) {
		t.Errorf("candidates for %q = %v, want [token]", "tok", got)
	}
	if got := completionCandidates([]string{""}); slices.Contains(got, "token") {
		t.Errorf("bare top-level list must not advertise alias token: %v", got)
	}
}

// TestCompletionSubcommands: a hand-dispatched verb completes its subcommand words (order doesn't
// matter — they come off a map, not a table); a leaf with no args/sub/flags (router start) offers
// nothing, so the shell falls back to filenames.
func TestCompletionSubcommands(t *testing.T) {
	got := completionCandidates([]string{"secrets", "s"})
	slices.Sort(got)
	if want := []string{"scan", "set", "status"}; !slices.Equal(got, want) {
		t.Errorf("secrets s* = %v, want %v", got, want)
	}
	if got := completionCandidates([]string{"memory", "rev"}); !slices.Equal(got, []string{"review"}) {
		t.Errorf("memory rev* = %v, want [review]", got)
	}
	if got := completionCandidates([]string{"router", "start", ""}); len(got) != 0 {
		t.Errorf("leaf command should offer no candidates, got %v", got)
	}
}

// TestGeminiSubverbsCompleteDespiteBareLauncherGuard: gemini is a bare launcher (noComplete), but its
// hand-dispatched ask/bridge subwords are safe (in-process) and must still complete normally.
func TestGeminiSubverbsCompleteDespiteBareLauncherGuard(t *testing.T) {
	if got := completionCandidates([]string{"gemini", "a"}); !slices.Equal(got, []string{"ask"}) {
		t.Errorf("gemini a* = %v, want [ask]", got)
	}
	got := completionCandidates([]string{"gemini", "bridge", "--"})
	if !slices.Contains(got, "--task") || !slices.Contains(got, "--rubric") {
		t.Errorf("gemini bridge flags = %v, missing --task/--rubric", got)
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
	if got := completionCandidates([]string{"theme", "--"}); !slices.Equal(got, []string{"--preview"}) {
		t.Errorf("theme -- = %v, want [--preview]", got)
	}
	if got := completionCandidates([]string{"doctor", "--"}); !slices.Equal(got, []string{"--audit", "--days", "--format"}) {
		t.Errorf("doctor -- = %v, want [--audit --days --format]", got)
	}
}

// TestCompletionFlagValues: the word after a value flag completes from its domain; a flag with no
// registered domain (--model) offers nothing (filenames fallback), not an error.
func TestCompletionFlagValues(t *testing.T) {
	if got := completionCandidates([]string{"switch", ""}); !slices.Equal(got, providerNames()) {
		t.Errorf("switch values = %v, want %v", got, providerNames())
	}
	if got := completionCandidates([]string{"theme", ""}); !slices.Equal(got, append([]string{"apply"}, statusline.GradientNames()...)) {
		t.Errorf("theme values = %v, want apply + %v", got, statusline.GradientNames())
	}
	if got := completionCandidates([]string{"ask", "--agent", ""}); !slices.Equal(got, agentNames()) {
		t.Errorf("ask --agent values = %v, want %v", got, agentNames())
	}
	if got := completionCandidates([]string{"ask", "--model", ""}); len(got) != 0 {
		t.Errorf("ask --model values = %v, want none (no registered domain)", got)
	}
}

// TestCompletionPositionals: a verb's first bare argument completes from its own static domain.
func TestCompletionPositionals(t *testing.T) {
	if got := completionCandidates([]string{"run", ""}); !slices.Equal(got, vendorNames()) {
		t.Errorf("run values = %v, want %v", got, vendorNames())
	}
	if got := completionCandidates([]string{"hook", ""}); !slices.Equal(got, hookNames()) {
		t.Errorf("hook values = %v, want %v", got, hookNames())
	}
	if got := completionCandidates([]string{"workflow", "skeleton", ""}); !slices.Equal(got, workflowPatterns()) {
		t.Errorf("workflow skeleton values = %v, want %v", got, workflowPatterns())
	}
	if got := completionCandidates([]string{"profile", ""}); !slices.Equal(got, providerNames()) {
		t.Errorf("profile values = %v, want %v", got, providerNames())
	}
	// "run codex --" is past the vendor's own (unknown) flags — must offer nothing, not the run
	// verb's positional domain again.
	if got := completionCandidates([]string{"run", "codex", "--"}); len(got) != 0 {
		t.Errorf("run codex -- = %v, want none (codex's own flags are not ours to know)", got)
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
	for _, name := range []string{"opus", "sonnet", "haiku", "fable", "codex", "copilot", "opencode", "gemini", "run", "science", "claude"} {
		if n := specTree()[name]; n != nil && len(n.flags) != 0 {
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

// TestNoCompleteNamesAreRealVerbs guards the noComplete table against naming a verb that was renamed
// or removed, which would silently stop protecting it.
func TestNoCompleteNamesAreRealVerbs(t *testing.T) {
	for name := range noComplete {
		if lookup(name) == nil {
			t.Errorf("noComplete lists %q, which is not a registered verb", name)
		}
	}
}

// TestCompleteDispatch: the hidden __complete verb prints one candidate per line.
func TestCompleteDispatch(t *testing.T) {
	code, out, _ := captureOutput(t, func() int { return cmdComplete([]string{"me"}) })
	if code != 0 {
		t.Fatalf("__complete me exit %d", code)
	}
	if strings.TrimSpace(out) != "memory" {
		t.Errorf("__complete me = %q, want memory", out)
	}
}

// TestCompletionScript: the emitted script registers only the harness binary.
func TestCompletionScript(t *testing.T) {
	code, out, _ := captureOutput(t, func() int { return cmdCompletion([]string{"bash"}) })
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
	_, out, _ := captureOutput(t, func() int { return cmdCompletion([]string{"bash"}) })
	if err := writeFile(script, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	argvFile := filepath.Join(dir, "argv")
	stub := "#!" + bash + "\nprintf '%s\\n' \"$@\" > " + shQuote(argvFile) + "\necho codex\n"
	stubPath := filepath.Join(dir, "claude-code")
	if err := writeFile(stubPath, stub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only words through COMP_CWORD reach the harness, excluding words after the cursor.
	drive := "source " + shQuote(script) + "\n" +
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
	code, out, _ := captureOutput(t, func() int { return cmdCompletion([]string{"--install"}) })
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
	if code, _, _ := captureOutput(t, func() int { return cmdCompletion([]string{"--install"}) }); code != 0 {
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
		code, out, _ := captureOutput(t, func() int { return cmdCompletion([]string{flagArg}) })
		if code != 0 {
			t.Errorf("completion %s exit %d, want 0", flagArg, code)
		}
		if !strings.Contains(out, completionUsage) {
			t.Errorf("completion %s missing usage line:\n%s", flagArg, out)
		}
	}
	if _, err := readFile(filepath.Join(dir, "completions", "claude-code")); err == nil {
		t.Error("-h/--help must not install anything")
	}
}

// TestCompletionUnknownArgExitsTwo: an unrecognized argument is a usage error, not a silent no-op.
func TestCompletionUnknownArgExitsTwo(t *testing.T) {
	if code, _, _ := captureOutput(t, func() int { return cmdCompletion([]string{"--bogus"}) }); code != 2 {
		t.Errorf("completion --bogus exit %d, want 2", code)
	}
}

// writeFile is os.WriteFile with the test's own name, kept beside the completion tests that use it.
func writeFile(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}

// shQuote single-quotes s for a bash script literal.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// currentPath is the test process's PATH, so a stub dir can be prepended without losing bash's tools.
func currentPath() string { return os.Getenv("PATH") }

// readFile returns path's contents as a string.
func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// TestCompletionMemoryMigrate: the migrate verb and its flags complete; --format offers its two values.
func TestCompletionMemoryMigrate(t *testing.T) {
	if got := completionCandidates([]string{"memory", "mi"}); !slices.Equal(got, []string{"migrate"}) {
		t.Errorf("memory mi* = %v, want [migrate]", got)
	}
	got := completionCandidates([]string{"memory", "migrate", "--"})
	for _, want := range []string{"--dry-run", "--manifest", "--origin", "--format"} {
		if !slices.Contains(got, want) {
			t.Errorf("memory migrate flags missing %q: %v", want, got)
		}
	}
	if got := completionCandidates([]string{"memory", "migrate", "--format", ""}); !slices.Equal(got, []string{"text", "json"}) {
		t.Errorf("memory migrate --format values = %v", got)
	}
}
