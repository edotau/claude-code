package codex

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/permission"
	"github.com/edotau/claude-code/internal/platform/shell"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// codexDenial names the layer that refused a failed shell call and its real control.
func codexDenial(command, output string, tools bool, sandbox string) (permission.Denial, bool) {
	call := permission.Rule{Tool: "Bash", Arg: codexShellCommand(command)}
	text := strings.ToLower(output)
	denial := func(layer, source, remedy string) (permission.Denial, bool) {
		return permission.Denial{Call: call, Layer: layer, Source: source, Remedy: remedy}, true
	}
	switch {
	case strings.Contains(text, "blocked by pretooluse hook"):
		return denial("hook", "safety hook", "a safety block has no override; report it instead of retrying another way")
	case strings.Contains(text, "network access was denied by the codex sandbox"), strings.Contains(text, "network access was blocked by policy"):
		if !tools {
			return denial("os-sandbox", "read-only sandbox", "relaunch with --tools; the read-only sandbox has no network")
		}
		return denial("os-sandbox", "sandbox network", `relaunch with overlay {"codexNetwork":true} to open sandbox network (all egress)`)
	case strings.Contains(text, "blocked by policy"):
		return denial("codex-rules", "$CODEX_HOME/rules", "a rule file outside this dispatch forbids it; edit that rule")
	case textutil.ContainsAny(text, "operation not permitted", "read-only file system", "denied by sandbox", "sandbox denied"):
		switch {
		case !tools:
			return denial("os-sandbox", "read-only sandbox", "relaunch with --tools; the read-only sandbox cannot write")
		case sandbox == "workspace-write":
			return denial("os-sandbox", "workspace-write sandbox", "this run is confined to workspace-write; set the codex record sandboxMode to danger-full-access (writes anywhere) only if the write is intended")
		}
		return denial("os-sandbox", sandbox+" sandbox", "the OS refused beyond the sandbox; no harness control repairs it")
	}
	return permission.Denial{}, false
}

// codexShellCommand is the command inside codex's `bash -lc '<cmd>'` wrapper, else command as given.
func codexShellCommand(command string) string {
	toks := shell.Tokens(command)
	for i := 0; i+1 < len(toks); i++ {
		if toks[i] == "-lc" || toks[i] == "-c" {
			return toks[i+1]
		}
	}
	return command
}

const (
	denialCommandLimit = 3
	denialLabelRunes   = 80
)

func (s *denialSink) runStream(ctx context.Context, name string, cmd *exec.Cmd, tail *retry.TailBuffer, res wire.Result) (wire.Result, error) {
	idle := agent.NewIdleTimeout(name, agent.IdleLimitFor(name), nil)
	res, err := agent.RunStreamWith(ctx, name, cmd, tail, res, s, parseCodexIdle(idle), agent.StreamOpts{Idle: idle})
	res.Denials = s.denials
	return res, err
}

func (s *denialSink) collect(d permission.Denial, ev wire.Event) {
	if d.Layer != "os-sandbox" {
		return
	}
	if s.denials == nil {
		s.denials = &wire.Denials{}
	}
	s.denials.Count++
	if len(s.denials.Commands) >= denialCommandLimit {
		return
	}
	label := "code mode"
	if ev.Type == wire.EventToolResult {
		label = denialLabel(s.commands[ev.Tool.ID])
	}
	if label != "" {
		s.denials.Commands = append(s.denials.Commands, label)
	}
}

var labelShells = []string{"bash", "sh", "zsh", "dash", "ksh", "fish"}

var labelSubcommandTools = []string{"git", "go", "make", "npm", "pnpm", "yarn", "cargo", "docker", "kubectl", "pip", "pip3", "gh", "helm", "terraform", "brew"}

// labelSecond reports whether word is a flag of one or two letters or a lower-case subcommand of a known tool.
func labelSecond(verb, word string) bool {
	if rest, ok := strings.CutPrefix(word, "-"); ok {
		return len(rest) > 0 && len(rest) <= 2 && !strings.ContainsFunc(rest, func(r rune) bool { return !unicode.IsLetter(r) })
	}
	return slices.Contains(labelSubcommandTools, verb) &&
		!strings.ContainsFunc(word, func(r rune) bool { return r != '-' && (r < 'a' || r > 'z') })
}

// denialLabel is a command's verb plus a short flag or a known tool's subcommand: no value, path or address reaches it.
func denialLabel(command string) string {
	words := labelWords(command)
	if len(words) == 0 {
		return ""
	}
	label := filepath.Base(words[0])
	if len(words) > 1 && labelSecond(label, words[1]) {
		label += " " + words[1]
	}
	runes := []rune(label)
	return string(runes[:min(denialLabelRunes, len(runes))])
}

// labelWords unwraps a shell -c/-lc (also behind env or rtk) and drops leading NAME=value words.
func labelWords(command string) []string {
	words := shell.Tokens(command)
	for range 3 {
		var done bool
		if words, done = unwrapLabel(dropAssignments(words)); done {
			return words
		}
	}
	return words
}

// unwrapLabel peels one rtk or shell -c layer off words; done means words is the real command.
func unwrapLabel(words []string) ([]string, bool) {
	if len(words) > 1 && filepath.Base(words[0]) == "rtk" { // the harness's command wrapper hides the real verb
		words = words[1:]
		if words[0] == "proxy" && len(words) > 1 {
			words = words[1:]
		}
		return words, false
	}
	words = dropEnvShell(words)
	if len(words) == 0 || !slices.Contains(labelShells, filepath.Base(words[0])) {
		return words, true
	}
	i := slices.IndexFunc(words, func(w string) bool { return w == "-c" || w == "-lc" })
	if i < 0 || i+1 >= len(words) {
		return words, true
	}
	return shell.Tokens(words[i+1]), false
}

// dropEnvShell strips an `env NAME=v` prefix when a shell follows it.
func dropEnvShell(words []string) []string {
	if len(words) == 0 || filepath.Base(words[0]) != "env" {
		return words
	}
	if rest := dropAssignments(words[1:]); len(rest) > 0 && slices.Contains(labelShells, filepath.Base(rest[0])) {
		return rest
	}
	return words
}

func dropAssignments(words []string) []string {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	return words
}

func isAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	return ok && name != "" && !strings.ContainsFunc(name, func(r rune) bool {
		return r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
	})
}

// denialSink preserves events and warns about the layer and control behind refusals.
type denialSink struct {
	denials  *wire.Denials
	next     wire.Sink
	tools    bool
	sandbox  string
	commands map[string]string // tool id -> command
	warned   bool              // one OS-sandbox warning per run
}

func (s *denialSink) Emit(ev wire.Event) error {
	if err := s.next.Emit(ev); err != nil {
		return err
	}
	d, ok := s.denial(ev)
	if !ok {
		return nil
	}
	s.collect(d, ev)
	s.warned = s.warned || d.Layer == "os-sandbox"
	what := d.Call.String()
	if d.Call.Arg == "" {
		what = "a command"
	}
	return s.next.Emit(wire.Event{Type: wire.EventProgress, Status: "warning",
		Text: fmt.Sprintf("codex: %s refused %s (%s): %s", d.Layer, what, d.Source, d.Remedy)})
}

// denial is the refusal ev reports, if any; a tool use is remembered so its result can name the command.
func (s *denialSink) denial(ev wire.Event) (d permission.Denial, ok bool) {
	switch {
	case ev.Type == wire.EventToolUse && ev.Tool != nil:
		s.commands[ev.Tool.ID] = ev.Text
	case ev.Type == wire.EventToolResult && ev.Tool != nil && !strings.HasPrefix(ev.Status, "completed"):
		d, ok = codexDenial(s.commands[ev.Tool.ID], ev.Text, s.tools, s.sandbox)
	case ev.Type == wire.EventText && !s.warned:
		if d, ok = codexDenial("", ev.Text, s.tools, s.sandbox); ok && d.Layer != "os-sandbox" {
			ok = false // prose names a refusal only by its OS signature
		}
	}
	return d, ok
}
