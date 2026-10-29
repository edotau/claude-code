package pathview

import (
	"strconv"
	"strings"
)

// parseMountinfo reads /proc/self/mountinfo text into classified mounts.
func parseMountinfo(text string) []Mount {
	var out []Mount
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		sep := indexOf(f, "-")
		if len(f) < 7 || sep < 6 || len(f) < sep+3 {
			continue
		}
		if m, ok := classify(unescapeOctal(f[4]), unescapeOctal(f[sep+2]), f[sep+1]); ok {
			out = append(out, m)
		}
	}
	return out
}

// parseDarwinMount reads macOS `mount` output ("src on /point (fstype, opts)") into classified mounts.
func parseDarwinMount(text string) []Mount {
	var out []Mount
	for _, line := range strings.Split(text, "\n") {
		src, rest, ok := strings.Cut(line, " on ")
		open := strings.LastIndex(rest, " (")
		if !ok || open < 0 || !strings.HasSuffix(rest, ")") {
			continue
		}
		fstype, _, _ := strings.Cut(rest[open+2:len(rest)-1], ",")
		if m, ok := classify(rest[:open], src, strings.TrimSpace(fstype)); ok {
			out = append(out, m)
		}
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
