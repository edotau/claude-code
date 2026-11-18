package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/shell"
)

const (
	exfilSink = `(curl|nc|ncat|wget)([^[:alnum:]_]|$)`
	// The regex secretPipe is superseded by secretPiped; it stays while safety.go's exfilPatterns names it.
	secretCmd = `(?:claude-code\s+vault\s+get|security\s+find-(?:generic|internet)-password\s[^|;&\n]*-[a-zA-Z]*w|` +
		`gh\s+auth\s+token|databricks\s+auth\s+token|oc\s+whoami\s+-t|gcloud\s+auth\s+print-(?:access|identity)-token|` +
		`az\s+account\s+get-access-token|aws\s+ecr\s+get-login-password|aws\s+sts\s+get-session-token|` +
		`kubectl\s+config\s+view\s[^|;&\n]*--raw)`
	stageTail = `(?:[^|;&\n]|>&|&>)*`
	secretSrc = cmdStart + `(?:\S*/)?(?:` + secretCmd + stageTail +
		`|(?:env(?:\s+-\S+)*|printenv(?:\s+\w+)*)\s*)`
	secretSink = `\|&?[ \t]*(?:xargs\s+(?:-[^Ii\s]\S*\s+)*|env\s+|command\s+|\\)?(?:\S*/)?` + exfilSink
	secretPipe = secretSrc + `(?:\|&?[ \t]*[^|;&\n \t]` + stageTail + `)*` + secretSink
)

// credentialCmds print a credential: per tool, its subcommand words in order; flags and their values may sit between.
var credentialCmds = map[string][][]string{
	"claude-code": {{"vault", "get"}}, "gh": {{"auth", "token"}}, "databricks": {{"auth", "token"}},
	"gcloud": {{"auth", "print-access-token"}, {"auth", "print-identity-token"}}, // application-default sits between
	"az":     {{"account", "get-access-token"}},
	"aws": {{"ecr", "get-login-password"}, {"ecr-public", "get-login-password"}, {"sts", "get-session-token"},
		{"sts", "assume-role"}, {"sts", "get-federation-token"}, {"configure", "export-credentials"},
		{"sso", "get-role-credentials"}, {"secretsmanager", "get-secret-value"}},
	"kubectl":  {{"config", "view", "--raw"}, {"create", "token"}},
	"oc":       {{"whoami", "-t"}, {"whoami", "--show-token"}, {"config", "view", "--raw"}, {"create", "token"}, {"sa", "get-token"}},
	"security": {{"find-generic-password", "-w"}, {"find-internet-password", "-w"}},
}

// networkSinks send their input to another host; openssl counts only as s_client.
var networkSinks = []string{"curl", "wget", "nc", "ncat", "netcat", "socat", "telnet", "ssh", "scp", "sftp", "ftp"}

// secretMark stands in for a substitution whose command prints a credential, so its segment counts as a source.
const secretMark = "\x00secret"

// maxSecretNest bounds `sh -c`/`eval` and substitution recursion.
const maxSecretNest = 4

// secretPiped reports a credential command piped, through any stages, into a network sender, or a /dev/tcp socket.
func secretPiped(cmd string) bool { return pipesSecret(cmd, 0) }

func pipesSecret(cmd string, depth int) bool {
	if depth > maxSecretNest || !strings.Contains(cmd, "|") && !strings.Contains(cmd, "/dev/") {
		return false
	}
	cmd = markSubs(cmd, depth)
	socket := strings.Contains(cmd, "/dev/tcp/") || strings.Contains(cmd, "/dev/udp/")
	segs, seps := shell.SegmentsSep(cmd)
	carry, inGroup, open := false, false, 0
	for i, seg := range segs {
		toks := shell.Tokens(seg)
		if nested, ok := shell.NestedCommand(toks); ok && pipesSecret(nested, depth+1) || carry && sends(toks, depth) {
			return true
		}
		src := carry || strings.Contains(seg, secretMark) || printsSecret(toks, depth)
		if src && socket {
			return true
		}
		src, inGroup, open = grouped(seg, src, inGroup, open)
		carry = src && (seps[i] == "|" || seps[i] == "|&")
	}
	return false
}

// markSubs replaces each `$(…)`, backtick or `<(…)` whose command prints a credential with secretMark.
func markSubs(cmd string, depth int) string {
	_, subs := shell.ExtractSubstitutions(cmd)
	for _, sub := range subs {
		if printsAny(sub, depth+1) {
			for _, pair := range [][2]string{{"$(", ")"}, {"<(", ")"}, {"`", "`"}} {
				cmd = strings.Replace(cmd, pair[0]+sub+pair[1], " "+secretMark+" ", 1)
			}
		}
	}
	return cmd
}

// grouped carries a source out of a `{ …; }` or `( … )` group to the stage the group pipes into.
func grouped(seg string, src, inGroup bool, open int) (bool, bool, int) {
	t := strings.TrimSpace(seg)
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "(") {
		open++
	}
	if open == 0 {
		return src, false, 0
	}
	inGroup = inGroup || src
	if strings.HasSuffix(t, "}") || strings.HasSuffix(t, ")") {
		if open--; open == 0 {
			return inGroup, false, 0
		}
	}
	return src, inGroup, open
}

// printsAny reports any segment of text that prints a credential.
func printsAny(text string, depth int) bool {
	if depth > maxSecretNest {
		return false
	}
	text = markSubs(text, depth)
	return slices.ContainsFunc(shell.Segments(text), func(seg string) bool {
		return strings.Contains(seg, secretMark) || printsSecret(shell.Tokens(seg), depth)
	})
}

// printsSecret reports a command word of toks, past wrappers and runners, that prints a credential or the environment.
func printsSecret(toks []string, depth int) bool {
	if nested, ok := shell.NestedCommand(toks); ok {
		return printsAny(nested, depth+1)
	}
	return slices.ContainsFunc(shell.CommandStarts(toks), func(st []string) bool {
		w, _ := shell.Unwrap(st, "")
		if len(w) == 0 {
			return len(st) > 0 && filepath.Base(st[0]) == "env" // env with no command prints the environment
		}
		name := filepath.Base(w[0])
		return name == "printenv" || slices.ContainsFunc(credentialCmds[name], func(words []string) bool { return inOrder(w[1:], words) })
	})
}

// inOrder reports words appearing in args in order; a one-letter flag also matches inside a short cluster.
func inOrder(args, words []string) bool {
	for _, a := range args {
		if len(words) == 0 {
			break
		}
		w := words[0]
		name, _, _ := strings.Cut(a, "=")
		if name == w || len(w) == 2 && w[0] == '-' && len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Contains(a, w[1:]) {
			words = words[1:]
		}
	}
	return len(words) == 0
}

// sends reports a command word of toks, past wrappers, runners and `sh -c`, that sends its input to another host.
func sends(toks []string, depth int) bool {
	if nested, ok := shell.NestedCommand(toks); ok {
		return depth < maxSecretNest && slices.ContainsFunc(shell.Segments(nested), func(seg string) bool {
			return sends(shell.Tokens(seg), depth+1)
		})
	}
	for _, st := range shell.CommandStarts(toks) {
		w, _ := shell.Unwrap(st, "")
		if len(w) == 0 {
			continue
		}
		name, _, _ := strings.Cut(filepath.Base(w[0]), ".") // nc.openbsd is nc
		if name == "xargs" && xargsReplaces(w[1:]) {
			return false // `-I{}` puts the token in an argument, as a header does: allowed like `$(…)` in one
		}
		if slices.Contains(networkSinks, name) || name == "openssl" && slices.Contains(w[1:], "s_client") {
			return true
		}
	}
	return false
}

// xargsReplaces reports a leading -I/-i/--replace option of xargs.
func xargsReplaces(opts []string) bool {
	for _, o := range opts {
		if !strings.HasPrefix(o, "-") {
			return false
		}
		if strings.HasPrefix(o, "-I") || strings.HasPrefix(o, "-i") || strings.HasPrefix(o, "--replace") {
			return true
		}
	}
	return false
}
