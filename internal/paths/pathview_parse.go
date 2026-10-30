package paths

import (
	"path"
	"strconv"
	"strings"
)

// parseMountinfo reads /proc/self/mountinfo text into classified mounts.
func parseMountinfo(text string) []Mount {
	var out []Mount
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		sep := indexOf(f, "-")
		if len(f) < 7 || sep < 6 || len(f) < sep+3 {
			continue
		}
		point, source := unescapeOctal(f[4]), unescapeOctal(f[sep+2])
		m, ok := classify(point, source, f[sep+1])
		if !ok {
			m = Mount{Point: point, Source: source, FSType: f[sep+1], Kind: KindOpaque}
		}
		out = append(out, withRoot(m, unescapeOctal(f[3])))
	}
	return out
}

// withRoot applies mountinfo's root-within-source (field 4) so bind mounts of a subtree map correctly.
func withRoot(m Mount, root string) Mount {
	root = path.Clean("/" + root)
	switch m.Kind {
	case KindDrvFS:
		m.Root = root
	case KindSMB:
		m.RemotePath = strings.TrimPrefix(path.Join(m.RemotePath, root), "/")
	case KindNFS, KindSSHFS:
		m.RemotePath = path.Join(m.RemotePath, root)
	}
	return m
}

// parseDarwinMount reads macOS `mount` output ("src on /point (fstype, opts)") into classified mounts.
func parseDarwinMount(text string) []Mount {
	var out []Mount
	for line := range strings.SplitSeq(text, "\n") {
		src, rest, ok := strings.Cut(line, " on ")
		open := strings.LastIndex(rest, " (")
		if !ok || open < 0 || !strings.HasSuffix(rest, ")") {
			continue
		}
		fstype, _, _ := strings.Cut(rest[open+2:len(rest)-1], ",")
		m, ok := classify(rest[:open], src, strings.TrimSpace(fstype))
		if !ok {
			m = Mount{Point: rest[:open], Source: src, FSType: strings.TrimSpace(fstype), Kind: KindOpaque}
		}
		out = append(out, m)
	}
	return out
}

func indexOf(f []string, s string) int {
	for i, v := range f {
		if v == s {
			return i
		}
	}
	return -1
}

// unescapeOctal decodes the kernel's \040-style escapes in mountinfo fields.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
