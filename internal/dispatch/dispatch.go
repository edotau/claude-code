// Package dispatch builds the ssh/tmux command that starts an agent TUI on another tailnet machine
// in a detached tmux session, so a laptop or phone SSH client can attach to it later.
package dispatch

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/tailscale"
)

// Agents lists the allowed --agent values, in the order they're tried/offered.
var Agents = []string{"claude", "codex", "opencode", "gemini", "copilot"}

// ValidAgent reports whether name is one of Agents.
func ValidAgent(name string) bool {
	for _, a := range Agents {
		if a == name {
			return true
		}
	}
	return false
}

// nameRe is the --name / session-name allowlist: tmux session names embed in shell text we build.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidName reports whether name matches ^[A-Za-z0-9._-]{1,64}$.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// QuoteSH single-quotes s for POSIX sh, closing/reopening the quote around each embedded quote.
func QuoteSH(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// DefaultName returns "cc-<agent>-<id>" with a short random hex id.
func DefaultName(agent string) string {
	return fmt.Sprintf("cc-%s-%s", agent, randID())
}

func randID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return fmt.Sprintf("%x", b)
}

// Opts configures one dispatch's remote command.
type Opts struct {
	Dir   string // remote working directory; "" means the login home (~)
	Agent string // one of Agents
	Name  string // tmux session name; "" generates one from DefaultName
	Task  string // the initial prompt handed to the agent
}

// agentLaunchArgs is the agent CLI's own argv for running Task as its initial prompt, verified via
// `<agent> --help`: claude/codex take a bare positional prompt, opencode wants --prompt, gemini and
// copilot use -i/--interactive to seed a prompt and stay in the TUI.
func agentLaunchArgs(agent, task string) []string {
	switch agent {
	case "opencode":
		return []string{"--prompt", task}
	case "gemini", "copilot":
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
		quoted[i] = QuoteSH(a)
	}
	bare := agent + " " + strings.Join(quoted, " ")
	if agent == "claude" {
		return bare
	}
	shimmed := "claude-code " + agent + " " + strings.Join(quoted, " ")
	return "if command -v claude-code >/dev/null 2>&1; then " + shimmed + "; else " + bare + "; fi"
}

// BuildRemoteCommand returns the POSIX sh script to run over ssh: it checks tmux exists, then starts
// a detached session named opts.Name (or a generated one) in opts.Dir running the agent command.
func BuildRemoteCommand(opts Opts) (string, string, error) {
	if !ValidAgent(opts.Agent) {
		return "", "", fmt.Errorf("dispatch: --agent %q must be one of %s", opts.Agent, strings.Join(Agents, "|"))
	}
	name := opts.Name
	if name == "" {
		name = DefaultName(opts.Agent)
	}
	if !ValidName(name) {
		return "", "", fmt.Errorf("dispatch: --name %q must match %s", name, nameRe.String())
	}
	dir := opts.Dir
	if dir == "" {
		dir = "~"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "if ! command -v tmux >/dev/null 2>&1; then echo %s >&2; exit 1; fi\n",
		QuoteSH("dispatch: tmux not found on PATH"))
	fmt.Fprintf(&b, "tmux new-session -d -s %s -c %s sh -c %s\n",
		QuoteSH(name), QuoteSH(dir), QuoteSH(agentCommand(opts.Agent, opts.Task)))
	return b.String(), name, nil
}

// listCommand is the ssh-side command for --list: cc-* tmux sessions, one tab-separated row each.
const listCommand = `tmux list-sessions -F '#{session_name}\t#{session_created}\t#{session_attached}\t#{pane_current_command}' 2>/dev/null | awk -F'\t' '$1 ~ /^cc-/'`

// ListCommand returns the remote command --list runs.
func ListCommand() string { return listCommand }

// Session is one cc-* tmux session reported by --list.
type Session struct {
	Name       string
	Created    string
	Attached   bool
	CurrentCmd string
}

// ParseSessions parses listCommand's tab-separated output.
func ParseSessions(output string) []Session {
	var sessions []Session
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		s := Session{Name: cols[0]}
		if len(cols) > 1 {
			s.Created = cols[1]
		}
		if len(cols) > 2 {
			s.Attached = cols[2] == "1"
		}
		if len(cols) > 3 {
			s.CurrentCmd = cols[3]
		}
		sessions = append(sessions, s)
	}
	return sessions
}

// SSHArgs is the argv for running remoteCmd on host via the system ssh binary (no local shell).
func SSHArgs(host, remoteCmd string) []string {
	return []string{"ssh", host, remoteCmd}
}

// AttachArgs is the argv for `ssh -t host tmux attach -t session`, meant to replace the caller's
// process so tmux owns the TTY directly.
func AttachArgs(host, session string) []string {
	return []string{"ssh", "-t", host, "tmux", "attach", "-t", session}
}

// AttachCommand prints the hint shown after a successful dispatch.
func AttachCommand(host, session string) string {
	return strings.Join(AttachArgs(host, session), " ")
}

// ResolveHost matches host against the tailnet's machine list (name or short name, case-insensitive)
// when one is available offline; a bare single-word host with no match gets a helpful error naming
// the known machines, since that's almost certainly a typo'd machine name rather than an ssh alias.
// Anything else — a dotted FQDN, user@host, an IP, or a host:port — passes through for ssh/DNS/
// ~/.ssh/config to resolve, and so does a bare word when the tailnet can't be listed at all.
// "localhost" always passes through unchanged: it is never a tailnet peer name.
func ResolveHost(ctx context.Context, host string) (string, error) {
	if host == "localhost" {
		return host, nil
	}
	machines, err := tailscale.Machines(ctx)
	if err != nil || len(machines) == 0 {
		return host, nil
	}
	for _, m := range machines {
		if strings.EqualFold(m.Name, host) {
			return m.Name, nil
		}
		if short, _, ok := strings.Cut(m.Name, "."); ok && strings.EqualFold(short, host) {
			return m.Name, nil
		}
	}
	if looksLikeBareName(host) {
		names := make([]string, len(machines))
		for i, m := range machines {
			names[i] = m.Name
		}
		return "", fmt.Errorf("dispatch: no tailnet machine matches %q; known machines: %s", host, strings.Join(names, ", "))
	}
	return host, nil
}

// looksLikeBareName is true for a single-word host: no dot, no '@', no ':', and not an IP literal.
func looksLikeBareName(host string) bool {
	if strings.ContainsAny(host, ".@:") {
		return false
	}
	return net.ParseIP(host) == nil
}
