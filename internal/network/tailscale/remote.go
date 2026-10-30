// The remote files build the ssh/tmux command that starts an agent TUI on another tailnet machine
// in a detached tmux session, so a laptop or phone SSH client can attach to it later.

package tailscale

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
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
		quoted[i] = QuoteSH(a)
	}
	bare := agent + " " + strings.Join(quoted, " ")
	if agent == "claude" {
		return bare
	}
	shimmed := "claude-code " + agent + " " + strings.Join(quoted, " ")
	return "if command -v claude-code >/dev/null 2>&1; then " + shimmed + "; else " + bare + "; fi"
}

// QuotePath single-quotes a remote path for POSIX sh; a leading ~ becomes the "$HOME" expansion, which quotes would block.
func QuotePath(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + QuoteSH(strings.TrimPrefix(p, "~/"))
	default:
		return QuoteSH(p)
	}
}

// dirFlag is tmux's `-c <dir>` flag text, or "" to omit it (ssh already lands in $HOME).
func dirFlag(dir string) string {
	if dir == "" {
		return ""
	}
	return "-c " + QuotePath(dir)
}

// BuildRemoteCommand returns the POSIX sh script to run over ssh: it checks tmux exists, then starts
// a detached session named opts.Name (or a generated one) in opts.Dir running the agent command
// through the user's own login shell (so PATH entries from .profile/.bashrc, e.g. mise/npm-installed
// agent CLIs, are on PATH) and keeps the session open under that shell once the agent exits.
func BuildRemoteCommand(opts RemoteOpts) (string, string, error) {
	if !ValidAgent(opts.Agent) {
		return "", "", fmt.Errorf("dispatch: --agent %q must be one of %s", opts.Agent, strings.Join(RemoteAgents, "|"))
	}
	name := opts.Name
	if name == "" {
		name = DefaultName(opts.Agent)
	}
	if !ValidName(name) {
		return "", "", fmt.Errorf("dispatch: --name %q must match %s", name, nameRe().String())
	}
	agentCmd := agentCommand(opts.Agent, opts.Task) + `; exec "${SHELL:-/bin/sh}" -l`
	var b strings.Builder
	fmt.Fprintf(&b, "if ! command -v tmux >/dev/null 2>&1; then echo %s >&2; exit 1; fi\n",
		QuoteSH("network remote: tmux not found on PATH"))
	parts := []string{"tmux", "new-session", "-d", "-s", QuoteSH(name)}
	if df := dirFlag(opts.Dir); df != "" {
		parts = append(parts, df)
	}
	parts = append(parts, `"${SHELL:-/bin/sh}"`, "-lc", QuoteSH(agentCmd))
	// Chained with && (not a separate statement): a duplicate session name makes new-session fail,
	// and the tag must not then land on whatever unrelated session already holds that name.
	fmt.Fprintln(&b, strings.Join(parts, " ")+" &&")
	// Tag the session with the agent it was launched for, so pane-status capture can tell a plain
	// shell dispatch apart from a shell left behind after the agent process exited.
	fmt.Fprintf(&b, "tmux set-option -t %s @cc_agent %s\n", QuoteSH(name), QuoteSH(opts.Agent))
	return b.String(), name, nil
}

// fieldSep separates columns in tmux -F format output. It cannot be a tab: tmux's format engine
// replaces a literal tab byte in expanded format text with "_" (verified against tmux 3.7c), so a
// tab survives neither sh's quoting (which never expands "\t" to a real tab) nor tmux's own output.
// "|" is safe for every field here: ValidName already forbids it in session names, pane ids are
// tmux-generated ("%N"), @cc_agent is one of the fixed RemoteAgents list, and pane_current_command is a
// program basename.
const fieldSep = "|"

// listCommand is the ssh-side command for --list: cc-* tmux sessions, one fieldSep-separated row each.
const listCommand = `tmux list-sessions -F '#{session_name}` + fieldSep + `#{session_created}` + fieldSep +
	`#{session_attached}` + fieldSep + `#{pane_current_command}` + fieldSep + `#{@cc_agent}' 2>/dev/null | awk -F'` +
	fieldSep + `' '$1 ~ /^cc-/'`

// ListCommand returns the remote command --list runs.
func ListCommand() string { return listCommand }

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
			s.Attached = cols[2] == "1"
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

// SSHArgs is the argv for running remoteCmd on host via the system ssh binary (no local shell). The
// remote command is always run as `sh -c <remoteCmd>` so it executes under POSIX sh regardless of
// the login shell ssh would otherwise hand it to (e.g. fish, which does not speak this syntax).
func SSHArgs(host, remoteCmd string) []string {
	return []string{"ssh", host, "sh -c " + QuoteSH(remoteCmd)}
}

// ValidHost refuses an ssh destination that ssh would parse as an option or that carries whitespace/control bytes.
func ValidHost(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

// EncodeScript wraps a POSIX sh script as a `sh -c` command: the script rides base64 (A-Za-z0-9+/=), so sh, bash
// and fish parse the wrapper identically whatever bytes the script holds.
func EncodeScript(script string) string {
	return `sh -c 'eval "$(printf %s ` + base64.StdEncoding.EncodeToString([]byte(script)) + ` | base64 -d)"'`
}

// StreamArgs is the argv for running script on host with stdin left free for the caller: BatchMode forbids prompts,
// ServerAliveInterval drops a dead link, and "--" stops host from being read as an ssh option.
func StreamArgs(host, script string) ([]string, error) {
	if err := ValidHost(host); err != nil {
		return nil, err
	}
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "--", host, EncodeScript(script)}, nil
}

// ExecArgs runs remoteCmd under sh locally when host is this machine, else over ssh; batch forbids ssh prompts.
func ExecArgs(ctx context.Context, host, remoteCmd string, batch bool) []string {
	switch {
	case IsLocalHost(ctx, host):
		return []string{"sh", "-c", remoteCmd}
	case batch: // a server-side caller has no TTY: a missing key must fail, never wait on a password prompt
		return []string{"ssh", "-o", "BatchMode=yes", host, "sh -c " + QuoteSH(remoteCmd)}
	}
	return SSHArgs(host, remoteCmd)
}

var (
	selfMu             sync.Mutex
	selfReady          bool
	selfNames          []string
	selfRetryAt        time.Time
	selfFailureBackoff = 30 * time.Second
	now                = time.Now
)

// IsLocalHost reports whether host names this machine: localhost, its hostname, or its tailnet name/IPs.
func IsLocalHost(ctx context.Context, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if _, at, ok := strings.Cut(h, "@"); ok {
		h = at
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	if n, err := os.Hostname(); err == nil {
		n = strings.ToLower(strings.TrimSuffix(n, "."))
		short, _, _ := strings.Cut(n, ".")
		if h != "" && (h == n || h == short) {
			return true
		}
	}
	selfMu.Lock()
	if selfReady || now().Before(selfRetryAt) {
		local := slices.Contains(selfNames, h)
		selfMu.Unlock()
		return local
	}
	selfMu.Unlock()
	st, err := Status(ctx)
	selfMu.Lock()
	defer selfMu.Unlock()
	if !selfReady {
		if err != nil {
			selfRetryAt = now().Add(selfFailureBackoff)
		} else {
			selfReady = true
			selfNames = append(selfNames, st.IPs...)
			if dns := strings.ToLower(strings.TrimSuffix(st.DNSName, ".")); dns != "" {
				short, _, _ := strings.Cut(dns, ".")
				selfNames = append(selfNames, dns, short)
			}
		}
	}
	return slices.Contains(selfNames, h)
}

// AttachArgs is the argv for `ssh -t host tmux attach -t session`, meant to replace the caller's
// process so tmux owns the TTY directly.
func AttachArgs(host, session string) []string {
	if IsLocalHost(context.Background(), host) {
		return []string{"tmux", "attach", "-t", session}
	}
	return []string{"ssh", "-t", host, "tmux", "attach", "-t", session}
}

// AttachCommand prints the hint shown after a successful dispatch.
func AttachCommand(host, session string) string {
	return strings.Join(AttachArgs(host, session), " ")
}

// ResolveHost matches host against the tailnet's machine list (full or short name, case-insensitive)
// when one is available offline, mapping a short machine name to its full MagicDNS name. Anything
// that doesn't match — a dotted FQDN, user@host, an IP, a host:port, an ~/.ssh/config alias, or any
// host when the tailnet can't be listed at all — passes through unchanged; ssh reports an unknown
// host itself, and rejecting here would wrongly block a valid ssh alias that isn't a tailnet peer.
func ResolveHost(ctx context.Context, host string) (string, error) {
	if host == "localhost" {
		return host, nil
	}
	machines, err := Machines(ctx)
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
	return host, nil
}
