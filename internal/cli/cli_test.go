package cli

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
)

func TestRunDoesNotDispatchArgvZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"codex"}) })
	if code != 0 || stderr != "" {
		t.Fatalf("Run exit = %d, stderr = %q, want 0 and no error", code, stderr)
	}
	_, want, _ := hosttest.CaptureOutput(t, func() int { usage(os.Stdout); return 0 })
	if stdout != want {
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

// Every group is present and every row has a summary.
func TestMenuRowsFit(t *testing.T) {
	want := map[string]bool{groupAgents: false, groupAuth: false, groupSession: false, groupDiagnostics: false, groupSettings: false}
	for _, c := range commands() {
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
	for _, name := range []string{"providers", "token"} {
		if lookup(name) != nil {
			t.Errorf("removed verb %s still dispatches", name)
		}
	}
}

// Every rendered usage line fits 80 columns (barring one unbreakable word) and continuations start at the description column.
func TestUsageRowsWrapAtDescriptionColumn(t *testing.T) {
	_, out, _ := hosttest.CaptureOutput(t, func() int { usage(os.Stdout); return 0 })
	const menuWidth = 80
	col := 2 + nameWidth
	for line := range strings.SplitSeq(out, "\n") {
		if utf8.RuneCountInString(line) > menuWidth && len(strings.Fields(line)) > 2 {
			t.Errorf("line over %d cols: %q", menuWidth, line)
		}
		if ind := len(line) - len(strings.TrimLeft(line, " ")); ind > 10 && ind != col {
			t.Errorf("continuation indent %d, want %d: %q", ind, col, line)
		}
	}
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(c.name) + ` +\S`).MatchString(out) {
			t.Errorf("row for %s missing or misaligned", c.name)
		}
	}
}

func menuGroup(out, group string) string {
	i := strings.Index(out, "\n"+group+"\n")
	if i < 0 {
		return ""
	}
	rest := out[i+len(group)+2:]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestMenuRegroupNetworkAuthMemorySession(t *testing.T) {
	_, out, _ := hosttest.CaptureOutput(t, func() int { usage(os.Stdout); return 0 })
	if !regexp.MustCompile(`(?m)^  network +\S`).MatchString(menuGroup(out, "AUTH")) {
		t.Errorf("network row missing from AUTH:\n%s", out)
	}
	if !regexp.MustCompile(`(?m)^  memory +\S`).MatchString(menuGroup(out, "SESSION")) {
		t.Errorf("memory row missing from SESSION:\n%s", out)
	}
	if regexp.MustCompile(`(?m)^  memory +\S`).MatchString(menuGroup(out, "SETTINGS")) {
		t.Errorf("memory still listed under SETTINGS")
	}
	for _, old := range []string{"tailscale", "web", "mcp", "generate", "vscode", "settings", "statusline", "route", "router", "secrets"} {
		if regexp.MustCompile(`(?m)^  ` + old + ` +\S`).MatchString(out) {
			t.Errorf("old verb %q still in the menu", old)
		}
	}
}

func TestOldVerbsAreUnknownCommands(t *testing.T) {
	for _, old := range []string{"tailscale", "web", "mcp", "generate", "vscode", "settings", "statusline", "route", "router", "secrets"} {
		if lookup(old) != nil {
			t.Errorf("lookup(%q) still resolves", old)
		}
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"claude-code", old}) })
		if code != 2 || !strings.Contains(stderr, "unknown command") {
			t.Errorf("%s: exit %d, stderr %q, want exit 2 unknown command", old, code, stderr)
		}
	}
}

// TestUseVerbRemoved: `use` was the old name of `switch`; it is no longer a verb or an alias.
func TestUseVerbRemoved(t *testing.T) {
	if lookup("use") != nil {
		t.Error("use still dispatches")
	}
	if c := lookup("switch"); c == nil {
		t.Error("switch does not dispatch")
	}
	if got := completionCandidates([]string{"us"}); slices.Contains(got, "use") {
		t.Errorf("completion offers use: %v", got)
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
		"hook": true, "clip": true, "history diff": true, "workflow lint": true,
		"vault delete": true, "vault edit": true, "test ramdisk": true,
	}
	if len(path) == 2 && slices.Contains([]string{"copilot", "codex", "gemini", "opencode"}, path[0]) &&
		slices.Contains([]string{"cancel", "catalog", "models", "resume", "on", "off"}, path[1]) {
		return true
	}
	return flagless[strings.Join(path, " ")]
}

// AUTH, SESSION, DIAGNOSTICS and SETTINGS list verbs A→Z; ANTHROPIC keeps its tier order.
func TestMenuGroupsAlphabetical(t *testing.T) {
	byGroup := map[string][]string{}
	group := ""
	for _, r := range menuRows() {
		if r.head != "" {
			group = r.head
		} else if r.name != "" {
			byGroup[group] = append(byGroup[group], strings.TrimSpace(r.name))
		}
	}
	for g := range alphabetical {
		if !slices.IsSorted(byGroup[g]) {
			t.Errorf("%s not alphabetical: %v", g, byGroup[g])
		}
	}
	if got := byGroup[groupAgents]; len(got) == 0 || got[0] != "anthropic" {
		t.Errorf("AGENTS must open with anthropic, got %v", got)
	}
}

func TestVaultMenuRow(t *testing.T) {
	count := 0
	for _, c := range commands() {
		if c.name != "vault" {
			continue
		}
		count++
		if c.group != groupAuth {
			t.Errorf("vault group = %q, want %q", c.group, groupAuth)
		}
		if c.summary == "" {
			t.Error("vault summary is empty")
		}
		if c.hidden || c.run == nil {
			t.Error("vault menu row is hidden or has no handler")
		}
	}
	if count != 1 {
		t.Errorf("commands() has %d vault rows, want 1", count)
	}
}

func TestVaultInAuthGroupHelp(t *testing.T) {
	rows := menuRows()
	group := ""
	found := false
	for _, r := range rows {
		if r.head != "" {
			group = r.head
			continue
		}
		if strings.TrimSpace(r.name) == "vault" {
			found = true
			if group != groupAuth {
				t.Errorf("vault appears under menu group %q, want %q", group, groupAuth)
			}
		}
	}
	if !found {
		t.Fatal("menuRows() has no vault row")
	}
}

func TestConfigVerbIsUnknown(t *testing.T) {
	vaulttest.UseFreshKey(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"claude-code", "config", "show"}) })
	if code != 2 {
		t.Errorf("Run([config show]) = %d, want 2 (unknown verb)", code)
	}
	if !strings.Contains(stderr, `unknown command "config"`) {
		t.Errorf("stderr = %q, want it to name config as unknown", stderr)
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test") // the moved batch/envelope tests reach the proxy
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

// A command printing past the pipe buffer must not deadlock: the audit hung 5m on a large settings render.
func TestCaptureOutputDrainsPastThePipeBuffer(t *testing.T) {
	want := 256 << 10
	code, stdout, _ := hosttest.CaptureOutput(t, func() int {
		if _, err := os.Stdout.Write(make([]byte, want)); err != nil {
			return 1
		}
		return 0
	})
	if code != 0 || len(stdout) != want {
		t.Fatalf("code %d, captured %d bytes, want %d", code, len(stdout), want)
	}
}
