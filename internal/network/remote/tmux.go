// The tmux files build the ssh/tmux command that starts an agent TUI on another machine
// in a detached tmux session, so a laptop or phone SSH client can attach to it later.

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

	"github.com/edotau/claude-code/internal/textutil"
)

// RemoteAgents lists the allowed --agent values, in the order they're tried/offered.
var RemoteAgents = []string{"claude", "codex", "opencode", "gemini", "copilot"}

// ValidAgent reports whether name is one of RemoteAgents.
func ValidAgent(name string) bool {
	return slices.Contains(RemoteAgents, name)
}

// nameRe is the --name / session-name allowlist: tmux session names embed in shell text we build.
var nameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) })

// ValidName reports whether name matches ^[A-Za-z0-9._-]{1,64}$.
func ValidName(name string) bool { return nameRe().MatchString(name) }

// DefaultName returns "cc-<agent>-<id>" with a short random hex id.
func DefaultName(agent string) string {
	return fmt.Sprintf("cc-%s-%s", agent, randID())
}

func randID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

// RemoteOpts configures one dispatch's remote command.
type RemoteOpts struct {
	Dir   string // remote working directory; "" means the login home (~)
	Agent string // one of RemoteAgents
	Name  string // tmux session name; "" generates one from DefaultName
	Task  string // the initial prompt handed to the agent
}

// agentLaunchArgs is the agent CLI's own argv for running Task as its initial prompt, verified via
// `<agent> --help`: claude/codex take a bare positional prompt, opencode wants --prompt, copilot uses
// -i/--interactive to seed a prompt and stay in the TUI; gemini (agy) takes the attached `-i=<task>` form.
func agentLaunchArgs(agent, task string) []string {
	switch agent {
	case "opencode":
		return []string{"--prompt", task}
	case "gemini":
		return []string{"-i=" + task}
	case "copilot":
		return []string{"-i", task}
	default: // claude, codex
		return []string{task}
	}
}

// agentCommand is the shell command tmux runs: it prefers the remote's `claude-code <agent>` verb
// (same forwarded args, routed through the harness's provider/model plumbing) when claude-code is on
// PATH there, else falls back to the bare agent binary. claude has no claude-code subcommand of its
// own (only an argv[0] shim), so it always runs bare.
func agentCommand(agent, task string) string {
	args := agentLaunchArgs(agent, task)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = textutil.ShellQuote(a)
	}
	bare := agent + " " + strings.Join(quoted, " ")
	if agent == "claude" {
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

// BuildRemoteCommand returns the POSIX sh script to run over ssh: it checks tmux exists, then starts
// a detached session named opts.Name (or a generated one) in opts.Dir running the agent command
// through the user's own login shell (so PATH entries from .profile/.bashrc, e.g. mise/npm-installed
// agent CLIs, are on PATH) and keeps the session open under that shell once the agent exits.
func BuildRemoteCommand(opts RemoteOpts) (string, string, error) {
	if !ValidAgent(opts.Agent) {
		return "", "", fmt.Errorf("dispatch: --agent %q must be one of %s", opts.Agent, strings.Join(RemoteAgents, "|"))
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
	// Chained with && (not a separate statement): a duplicate session name makes new-session fail,
	// and the tag must not then land on whatever unrelated session already holds that name.
	fmt.Fprintln(&b, strings.Join(parts, " ")+" &&")
	// Tag the session with the agent it was launched for, so pane-status capture can tell a plain
	// shell dispatch apart from a shell left behind after the agent process exited.
	fmt.Fprintf(&b, "tmux set-option -t %s @cc_agent %s\n", textutil.ShellQuote(name), textutil.ShellQuote(opts.Agent))
	return b.String(), name, nil
}

// sessionName returns opts.Name or a generated one, rejecting names outside the tmux allowlist.
func sessionName(opts RemoteOpts) (string, error) {
	name := opts.Name
	if name == "" {
		name = DefaultName(opts.Agent)
	}
	if !ValidName(name) {
		return "", fmt.Errorf("dispatch: --name %q must match %s", name, nameRe().String())
	}
	return name, nil
}

// fieldSep separates columns in tmux -F format output. It cannot be a tab: tmux's format engine
// replaces a literal tab byte in expanded format text with "_" (verified against tmux 3.7c), so a
// tab survives neither sh's quoting (which never expands "\t" to a real tab) nor tmux's own output.
// "|" is safe for every field here: ValidName already forbids it in session names, pane ids are
// tmux-generated ("%N"), @cc_agent is one of the fixed RemoteAgents list, and pane_current_command is a
// program basename.
const fieldSep = "|"

// listCommand is the ssh-side --list command: cc-* rows; a real tmux failure exits 1 with its output on stderr.
const listCommand = `out=$(tmux list-sessions -F '#{session_name}` + fieldSep + `#{session_created}` + fieldSep +
	`#{session_attached}` + fieldSep + `#{pane_current_command}` + fieldSep + `#{@cc_agent}' 2>&1) || { ` +
	`case $out in *"no server running"*|*"No such file or directory"*) out= ;; ` +
	`*) printf '%s\n' "$out" >&2; exit 1 ;; esac; }; ` +
	`printf '%s\n' "$out" | awk -F'` + fieldSep + `' '$1 ~ /^cc-/'`

// RemoteSession is one cc-* tmux session reported by --list.
type RemoteSession struct {
	Name       string
	Created    string
	Attached   bool
	CurrentCmd string
	Agent      string // the agent it was launched for, from the @cc_agent tmux user option
}

// ParseSessions parses listCommand's fieldSep-separated output.
func ParseSessions(output string) []RemoteSession {
	var sessions []RemoteSession
	for line := range strings.SplitSeq(strings.TrimRight(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, fieldSep)
		s := RemoteSession{Name: cols[0]}
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
