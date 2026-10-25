package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/helptext"
)

var helpFormatRow = regexp.MustCompile(`^  (\S(?:.*?\S)?) {2,}(\S.*)$`)
var helpFormatFlag = regexp.MustCompile(`^(?:-[A-Za-z0-9]|--[A-Za-z0-9][A-Za-z0-9_-]+)(?: \S+)?$`)

func renderHelpPage(page helptext.Page) string {
	var out bytes.Buffer
	page.Fprint(&out)
	return out.String()
}

func helpPageHasCommand(page helptext.Page, name string) bool {
	for _, row := range page.Commands {
		words := strings.Fields(row[0])
		if len(words) > 0 && words[0] == name {
			return true
		}
	}
	return false
}

func TestHelpFormat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	excluded := map[string]bool{
		// These launch external CLIs and deliberately forward help unchanged.
		"opus": true, "sonnet": true, "haiku": true, "fable": true, "science": true,
	}
	hidden := []string{
		"ask", "agents", "models", "router", "review", "workflow", "hook",
		"help-svg", "clip", "bmp-transcode", "completion",
	}
	var verbs []string
	for _, c := range commands() {
		if !c.hidden && !excluded[c.name] {
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

func checkHelpFormat(t *testing.T, path []string, parents []string) {
	t.Helper()
	argv := append([]string{"claude-code"}, path...)
	argv = append(argv, "-h")
	code, stdout, stderr := captureOutput(t, func() int { return Run(argv) })
	text := stdout + stderr
	if code != 0 {
		t.Errorf("%s: exit %d, want 0:\n%s", strings.Join(argv, " "), code, text)
	}
	first := ""
	for _, line := range strings.Split(text, "\n") {
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
	repeatsParent := false
	for _, parent := range parents {
		repeatsParent = repeatsParent || text == parent
	}
	var subs []string
	seenCommands, seenFlags := map[string]bool{}, map[string]bool{}
	section, rows := "", 0
	for _, line := range strings.Split(text, "\n") {
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
		word := strings.Fields(match[1])[0]
		switch section {
		case "COMMANDS":
			if seenCommands[word] {
				t.Errorf("duplicate command row %q", word)
				continue
			}
			seenCommands[word] = true
			subs = append(subs, word)
		case "FLAGS":
			if !helpFormatFlag.MatchString(match[1]) {
				t.Errorf("malformed flag label: %q", match[1])
			}
			// Run does not expose its FlagSet; validate generated rows for shape and uniqueness.
			if seenFlags[word] {
				t.Errorf("duplicate flag row %q", word)
			}
			seenFlags[word] = true
		}
	}
	flagless := map[string]bool{
		"hook": true, "clip": true, "history diff": true, "workflow lint": true,
	}
	if len(path) == 2 {
		switch path[0] {
		case "copilot", "codex", "gemini", "opencode":
			switch path[1] {
			case "cancel", "models", "resume":
				flagless[strings.Join(path, " ")] = true
			}
		}
	}
	if rows == 0 && !flagless[strings.Join(path, " ")] {
		t.Errorf("help has no rows under COMMANDS or FLAGS:\n%s", text)
	}
	// Parent-page help is intentional for some flagless actions; never dispatch its rows again.
	if repeatsParent || path[0] == "hook" {
		return
	}
	if len(path) >= 8 && len(subs) > 0 {
		t.Fatal("help command nesting exceeds eight levels")
	}
	for _, sub := range subs {
		t.Run(sub, func(t *testing.T) {
			if len(path) == 1 {
				switch path[0] {
				case "copilot", "codex", "gemini", "opencode":
					// These syntax rows launch an external CLI; they are not harness subcommands.
					if sub == "[launch" || sub == "--" {
						t.Skip("launch/passthrough syntax forwards -h to an external CLI")
					}
				}
			}
			checkHelpFormat(t, append(append([]string{}, path...), sub), append(parents, text))
		})
	}
}
