package hooks

import "strings"

// dropRemoteHeredocs removes heredoc bodies fed to ssh: they run on the remote host, not in this checkout.
func dropRemoteHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		toks := shellTokens(lines[i])
		m := heredocOp.FindStringSubmatch(lines[i])
		if m == nil || len(toks) == 0 || toks[0] != "ssh" || strings.Contains(lines[i], "|") {
			continue
		}
		delim, dash := m[2]+m[3]+m[4], m[1] == "-"
		for j := i + 1; j < len(lines); j++ {
			if l := lines[j]; l == delim || dash && strings.TrimLeft(l, "\t") == delim {
				i = j - 1
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
