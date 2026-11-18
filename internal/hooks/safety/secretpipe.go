package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// credentialCmds print a credential: per tool, its subcommand words in order (claude-code's is shell.SecretFlag).
var credentialCmds = map[string][][]string{
	"databricks": {{"auth", "token"}},
	"gh":         {{"auth", "token"}, {"auth", "status", "-t"}, {"auth", "status", "--show-token"}},
	"gcloud": {{"auth", "print-access-token"}, {"auth", "print-identity-token"}, // application-default sits between
		{"auth", "print-refresh-token"}},
	"az": {{"account", "get-access-token"}},
	"aws": {{"ecr", "get-login-password"}, {"ecr-public", "get-login-password"}, {"sts", "get-session-token"},
		{"sts", "assume-role"}, {"sts", "get-federation-token"}, {"configure", "export-credentials"},
		{"sso", "get-role-credentials"}, {"secretsmanager", "get-secret-value"}},
	"kubectl":  {{"config", "view", "--raw"}, {"create", "token"}},
	"oc":       {{"whoami", "-t"}, {"whoami", "--show-token"}, {"config", "view", "--raw"}, {"create", "token"}, {"sa", "get-token"}},
	"security": {{"find-generic-password", "-w"}, {"find-internet-password", "-w"}},
}

// networkSinks send their input to another host, for credential commands and files; openssl counts only as s_client.
var networkSinks = []string{"curl", "wget", "nc", "ncat", "netcat", "socat", "telnet", "ssh", "scp", "sftp", "ftp"}

// publicKey is a public key or known_hosts path under .ssh/: no secret, so credRead skips it (RE2 has no lookahead).
var publicKey = core.Gate(`\.ssh/(?:[^\s|;&<>'"]*\.pub|(?:[^\s|;&<>'"]*/)?known_hosts[^\s|;&<>'"]*)([\s|;&<>'"]|$)`, ".ssh/")

// secretMark stands in for a substitution whose command prints a credential, so its segment counts as a source.
const secretMark = "\x00secret"

// maxSecretNest bounds `sh -c`/`eval` and substitution recursion.
const maxSecretNest = 4

// secretPiped reports a credential command piped, through any stages, into a network sender, or a /dev/tcp socket.
func secretPiped(cmd string) bool { return pipesSecret(cmd, 0) }

func pipesSecret(cmd string, depth int) bool {
	if depth > maxSecretNest || !strings.ContainsAny(cmd, "|(`") && !strings.Contains(cmd, "/dev/") {
		return false
	}
	cmd = markSubs(cmd, depth)
	socket := strings.Contains(cmd, "/dev/tcp/") || strings.Contains(cmd, "/dev/udp/")
	segs, seps := shell.SegmentsSep(cmd)
	carry, whole, groupEnv, inGroup, open := false, false, false, false, 0
	for i, seg := range segs {
		toks := shell.Tokens(seg)
		if nested, ok := shell.NestedCommand(toks); ok && pipesSecret(nested, depth+1) || carry && sends(seg, depth, whole) {
			return true
		}
		prints, env := printsSecret(toks, depth)
		src := carry || strings.Contains(seg, secretMark) || prints
		if src && (socket || subSends(seg, depth, whole || env)) || fedMark(seg) && sends(seg, depth, false) {
			return true
		}
		src, inGroup, open = grouped(seg, src, inGroup, open)
		carry = src && (seps[i] == "|" || seps[i] == "|&")
		whole = carry && (whole || env || groupEnv)
		groupEnv = open > 0 && (groupEnv || env) // an env dump inside `{ …; }` or `( … )` leaves with the group
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
		if strings.Contains(seg, secretMark) {
			return true
		}
		prints, _ := printsSecret(shell.Tokens(seg), depth)
		return prints
	})
}

// printsSecret reports a command word of toks, past wrappers and runners, printing a credential; env: the environment.
func printsSecret(toks []string, depth int) (prints, env bool) {
	if nested, ok := shell.NestedCommand(toks); ok {
		return printsAny(nested, depth+1), false
	}
	for _, st := range shell.CommandStarts(toks) {
		w, _ := shell.Unwrap(st, "")
		if len(w) > 1 && w[0] == "builtin" { // bash's builtin prefix, which Unwrap keeps
			w, _ = shell.Unwrap(w[1:], "")
		}
		switch {
		case len(w) == 0 && len(st) > 0 && filepath.Base(st[0]) == "env", len(w) > 0 && filepath.Base(w[0]) == "printenv":
			return true, true // env with no command prints the environment
		case len(w) > 0 && slices.ContainsFunc(credentialCmds[filepath.Base(w[0])], func(words []string) bool { return inOrder(w[1:], words) }),
			shell.SecretFlag(shell.HarnessArgs(w)):
			return true, false
		}
	}
	return false, false
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

// sends reports a command word of seg (past wrappers, runners, sh -c, >(…)) sending its input on; whole: an env dump.
func sends(seg string, depth int, whole bool) bool {
	toks := shell.Tokens(seg)
	if nested, ok := shell.NestedCommand(toks); ok {
		return sendsAny(nested, depth, whole)
	}
	if subSends(seg, depth, whole) {
		return true
	}
	replaced := false
	for _, st := range shell.CommandStarts(toks) {
		w, _ := shell.Unwrap(st, "")
		if len(w) == 0 {
			continue
		}
		if replaced { // `-I{}` puts one token in an argument, as a header does; an env dump still goes whole
			if nested, ok := shell.NestedCommand(w); ok || !whole {
				return ok && shellPipesOn(nested, depth, whole)
			}
		}
		name, _, _ := strings.Cut(filepath.Base(w[0]), ".") // nc.openbsd is nc
		if name == "xargs" && xargsReplaces(w[1:]) {
			replaced = true
			continue
		}
		if slices.Contains(networkSinks, name) || name == "openssl" && slices.Contains(w[1:], "s_client") {
			return true
		}
	}
	return false
}

// subSends reports a `>(…)` of seg sending its input on: `tee >(curl …)` and `> >(nc …)`, not a `2> >(…)` stderr feed.
func subSends(seg string, depth int, whole bool) bool {
	if !strings.Contains(seg, ">(") {
		return false
	}
	_, subs := shell.ExtractSubstitutions(seg)
	return slices.ContainsFunc(subs, func(s string) bool {
		i := strings.Index(seg, ">("+s+")")
		pre := strings.TrimRight(seg[:max(i, 0)], " \t")
		return i >= 0 && !strings.HasSuffix(pre, "2>") && !strings.HasSuffix(pre, "2>>") && sendsAny(s, depth, whole)
	})
}

// fedMark reports a credential substitution fed to seg's stdin: `<<< "$(gh auth token)"` or `< <(gh auth token)`.
func fedMark(seg string) bool {
	return strings.Contains(seg, secretMark) && strings.Contains(strings.NewReplacer(" ", "", "\t", "", `"`, "").Replace(seg), "<"+secretMark)
}

// shellPipesOn reports an xargs -I `sh -c` text sending the token on: a piped segment, or any one for an env dump.
func shellPipesOn(text string, depth int, whole bool) bool {
	segs, seps := shell.SegmentsSep(text)
	for k := 0; depth < maxSecretNest && k < len(segs); k++ {
		if (whole || k > 0 && (seps[k-1] == "|" || seps[k-1] == "|&")) && sends(segs[k], depth+1, whole) {
			return true
		}
	}
	return false
}

// sendsAny reports any segment of text that sends its input to another host.
func sendsAny(text string, depth int, whole bool) bool {
	return depth < maxSecretNest && slices.ContainsFunc(shell.Segments(text), func(seg string) bool {
		return sends(seg, depth+1, whole)
	})
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
