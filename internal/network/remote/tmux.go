package remote

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/textutil"
)

// Agents lists the allowed --agent values, in the order they're tried/offered.
var Agents = []string{agentid.Claude, agentid.Codex, agentid.OpenCode, agentid.Gemini, agentid.Copilot}

// validAgent reports whether name is one of Agents.
func validAgent(name string) bool {
	return slices.Contains(Agents, name)
}

// nameRe is the --name / session-name allowlist: tmux session names embed in shell text we build.
var nameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) })

// validName reports whether name matches ^[A-Za-z0-9._-]{1,64}$.
func validName(name string) bool { return nameRe().MatchString(name) }

// defaultName returns "cc-<agent>-<id>" with a short random hex id.
func defaultName(agent string) string {
	return fmt.Sprintf("cc-%s-%s", agent, randID())
}

func randID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

// Opts configures one dispatch's remote command.
type Opts struct {
	Dir   string // remote working directory; "" means the login home (~)
	Agent string // one of Agents
	Name  string // tmux session name; "" generates one from defaultName
	Task  string // the initial prompt handed to the agent
}

// agentLaunchArgs is the agent's argv for an initial prompt: positional (claude, codex), --prompt, -i or -i=<task>.
func agentLaunchArgs(agent, task string) []string {
	switch agent {
	case agentid.OpenCode:
		return []string{"--prompt", task}
	case agentid.Gemini:
		return []string{"-i=" + task}
	case agentid.Copilot:
		return []string{"-i", task}
	default: // claude, codex
		return []string{task}
	}
}

// agentCommand prefers the remote's `claude-code <agent>` verb, else the bare binary; claude always runs bare.
func agentCommand(agent, task string) string {
	args := agentLaunchArgs(agent, task)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = textutil.ShellQuote(a)
	}
	bare := agent + " " + strings.Join(quoted, " ")
	if agent == agentid.Claude {
		return bare
	}
	shimmed := "claude-code " + agent + " " + strings.Join(quoted, " ")
	return "if command -v claude-code >/dev/null 2>&1; then " + shimmed + "; else " + bare + "; fi"
}

// dirFlag is tmux's `-c <dir>` flag text, or "" to omit it (ssh already lands in $HOME).
func dirFlag(dir string) string {
	if dir == "" {
		return ""
	}
	return "-c " + textutil.QuotePath(dir)
}

// buildCommand returns the sh script that starts a detached tmux session running the agent in a login shell.
func buildCommand(opts Opts) (string, string, error) {
	if !validAgent(opts.Agent) {
		return "", "", fmt.Errorf("--agent %q must be one of %s", opts.Agent, strings.Join(Agents, "|"))
	}
	name, err := sessionName(opts)
	if err != nil {
		return "", "", err
	}
	agentCmd := agentCommand(opts.Agent, opts.Task) + `; exec "${SHELL:-/bin/sh}" -l`
	var b strings.Builder
	fmt.Fprintf(&b, "if ! command -v tmux >/dev/null 2>&1; then echo %s >&2; exit 1; fi\n",
		textutil.ShellQuote("network remote: tmux not found on PATH"))
	parts := []string{"tmux", "new-session", "-d", "-s", textutil.ShellQuote(name)}
	if df := dirFlag(opts.Dir); df != "" {
		parts = append(parts, df)
	}
	parts = append(parts, `"${SHELL:-/bin/sh}"`, "-lc", textutil.ShellQuote(agentCmd))
	// Chained with &&: a duplicate name fails new-session, and the tag must not land on that other session.
	fmt.Fprintln(&b, strings.Join(parts, " ")+" &&")
	// Tag the session with its agent so pane status can tell a plain shell from one left after the agent exited.
	fmt.Fprintf(&b, "tmux set-option -t %s @cc_agent %s\n", textutil.ShellQuote(name), textutil.ShellQuote(opts.Agent))
	return b.String(), name, nil
}

// sessionName returns opts.Name or a generated one, rejecting names outside the tmux allowlist.
func sessionName(opts Opts) (string, error) {
	name := opts.Name
	if name == "" {
		name = defaultName(opts.Agent)
	}
	if !validName(name) {
		return "", fmt.Errorf("--name %q must match %s", name, nameRe().String())
	}
	return name, nil
}

// fieldSep separates tmux -F columns; not a tab, which tmux rewrites to "_". No field can hold "|".
const fieldSep = "|"

// listCommand is the ssh-side --list command: cc-* rows; a real tmux failure exits 1 with its output on stderr.
const listCommand = `out=$(tmux list-sessions -F '#{session_name}` + fieldSep + `#{session_created}` + fieldSep +
	`#{session_attached}` + fieldSep + `#{pane_current_command}` + fieldSep + `#{@cc_agent}' 2>&1) || { ` +
	`case $out in *"no server running"*|*"No such file or directory"*) out= ;; ` +
	`*) printf '%s\n' "$out" >&2; exit 1 ;; esac; }; ` +
	`printf '%s\n' "$out" | awk -F'` + fieldSep + `' '$1 ~ /^cc-/'`

// Session is one cc-* tmux session reported by --list.
type Session struct {
	Name       string
	Created    string
	Attached   bool
	CurrentCmd string
	Agent      string // the agent it was launched for, from the @cc_agent tmux user option
}

// parseSessions parses listCommand's fieldSep-separated output.
func parseSessions(output string) []Session {
	var sessions []Session
	for line := range strings.SplitSeq(strings.TrimRight(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, fieldSep)
		s := Session{Name: cols[0]}
		if len(cols) > 1 {
			s.Created = cols[1]
		}
		if len(cols) > 2 {
			clients, _ := strconv.Atoi(cols[2]) // a non-numeric count reads as detached
			s.Attached = clients >= 1
		}
		if len(cols) > 3 {
			s.CurrentCmd = cols[3]
		}
		if len(cols) > 4 {
			s.Agent = cols[4]
		}
		sessions = append(sessions, s)
	}
	return sessions
}
