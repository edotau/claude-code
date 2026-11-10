package textutil

import (
	"fmt"
	"strings"
)

// QuoteSH single-quotes s for POSIX sh unless it contains only shell-safe characters.
func QuoteSH(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./:@%+=,-", r)
	}) == -1 {
		return s
	}
	return ShellQuote(s)
}

// QuotePath quotes a remote POSIX sh path, expanding a leading ~ through the quoted HOME variable.
func QuotePath(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + ShellQuote(strings.TrimPrefix(p, "~/"))
	default:
		return ShellQuote(p)
	}
}

// ShellQuote always single-quotes s for POSIX sh, closing and reopening around embedded quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ValidHost refuses an ssh destination that ssh would parse as an option or that carries whitespace/control bytes.
func ValidHost(host string) error {
	bad := func(r rune) bool { return r <= ' ' || r == 0x7f }
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsFunc(host, bad) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}
