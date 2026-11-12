package paths

import (
	"context"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind classifies a mount by how its content is reached.
type Kind int

// Mount kinds.
const (
	kindOpaque Kind = iota // any other mount; kept so it fences off the mounts beneath it
	KindDrvFS              // WSL /mnt/<drive>, a Windows drive
	kindSMB                // //server/share (cifs, smbfs)
	KindNFS                // host:/export
	kindSSHFS              // user@host:path
)

// Mount is one classified mount-table entry.
type Mount struct {
	Point      string // local mount point
	Kind       Kind
	Drive      string // upper-case drive letter, KindDrvFS only
	Root       string // KindDrvFS: the drive subtree mounted here (bind mounts); "" means "/"
	Host       string // server host, remote kinds only
	RemotePath string // SMB: "share/sub" (no leading slash); NFS/sshfs: the exported path
}

var (
	liveOnce sync.Once
	live     *Resolver
)

// Default is the Resolver over this machine's live mount table, read once per process.
func Default() *Resolver {
	liveOnce.Do(func() {
		live = NewResolver(readMounts())
		if live.Distro == "" {
			live.Distro = learnDistro()
		}
	})
	return live
}

// classify turns a raw mount entry into a Mount; ok is false for filesystems that are not drvfs/remote.
func classify(point, source, fstype string) (Mount, bool) {
	norm := source // WSL drvfs exposes a UNC share as `\\server\share`; classify on the forward-slash form
	if strings.HasPrefix(source, `\\`) {
		norm = strings.ReplaceAll(source, `\`, "/")
	}
	m := Mount{Point: point}
	ok := true
	switch {
	case isDriveSource(norm):
		m.Kind, m.Drive = KindDrvFS, strings.ToUpper(norm[:1])
	case isSMB(norm, fstype):
		m.Kind = kindSMB
		m.Host, m.RemotePath, ok = splitSMB(norm)
	case strings.HasPrefix(fstype, "nfs"):
		m.Kind = KindNFS
		m.Host, m.RemotePath, ok = splitHostPath(norm)
	case isSSHFS(norm, fstype):
		m.Kind = kindSSHFS
		m.Host, m.RemotePath, ok = splitHostPath(norm)
	default:
		ok = false
	}
	if !ok {
		return Mount{}, false
	}
	return m, true
}

// isSSHFS matches an sshfs mount, or a fuse mount whose source names a user@host.
func isSSHFS(source, fstype string) bool {
	return strings.Contains(fstype, "sshfs") || (strings.Contains(fstype, "fuse") && strings.Contains(source, "@"))
}

// isDriveSource matches a WSL drvfs source: "C:" or "C:\".
func isDriveSource(s string) bool {
	if len(s) < 2 || len(s) > 3 || s[1] != ':' || !isLetter(s[0]) {
		return false
	}
	return len(s) == 2 || s[2] == '\\'
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func isSMB(source, fstype string) bool {
	switch fstype {
	case "cifs", "smb3", "smbfs", "smb":
		return true
	}
	return strings.HasPrefix(source, "//")
}

// splitSMB splits "//[user[:pw]@]host/share[/sub]" into host and "share[/sub]".
func splitSMB(source string) (host, share string, ok bool) {
	rest := strings.Trim(strings.TrimPrefix(source, "//"), "/")
	h, p, found := strings.Cut(rest, "/")
	if !found || h == "" || p == "" {
		return "", "", false
	}
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	return h, strings.Trim(p, "/"), h != ""
}

// splitHostPath splits "[user@]host:/path" into host and path; an IPv6 host may be bracketed.
func splitHostPath(source string) (host, p string, ok bool) {
	start := 0 // the user part ends at the last "@" before the first "/", which starts the path
	if before, _, found := strings.Cut(source, "/"); found {
		start = strings.LastIndex(before, "@") + 1
	}
	rest := source[start:]
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]:")
		if end < 0 {
			return "", "", false
		}
		host, p = rest[1:end], rest[end+2:]
	} else {
		var found bool
		if host, p, found = strings.Cut(rest, ":"); !found {
			return "", "", false
		}
	}
	if host == "" || !strings.HasPrefix(p, "/") {
		return "", "", false
	}
	return host, p, true
}

// wslpathOut runs `wslpath -w /`; a var so tests fake the probe.
var wslpathOut = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wslpath", "-w", "/")
	cmd.WaitDelay = 200 * time.Millisecond // a child holding stdout open must not outlive the probe
	out, err := cmd.Output()
	return string(out), err
}

// learnDistro asks wslpath for this distro's name when WSL_DISTRO_NAME is unset; "" on any failure.
var learnDistro = sync.OnceValue(func() string {
	out, err := wslpathOut()
	if err != nil {
		return ""
	}
	return distroFromUNC(out)
})

// distroFromUNC extracts <distro> from `\\wsl.localhost\<distro>\` or `\\wsl$\<distro>\`.
func distroFromUNC(out string) string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(out), `\`), `\`)
	if len(parts) < 2 || (!strings.EqualFold(parts[0], "wsl.localhost") && !strings.EqualFold(parts[0], "wsl$")) {
		return ""
	}
	return parts[1]
}

// parseMountinfo reads /proc/self/mountinfo text into classified mounts.
func parseMountinfo(text string) []Mount {
	var out []Mount
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if len(f) < 7 || sep < 6 || len(f) < sep+3 {
			continue
		}
		point, source := unescapeOctal(f[4]), unescapeOctal(f[sep+2])
		m, ok := classify(point, source, f[sep+1])
		if !ok {
			m = Mount{Point: point, Kind: kindOpaque}
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
	case kindSMB:
		m.RemotePath = strings.TrimPrefix(path.Join(m.RemotePath, root), "/")
	case KindNFS, kindSSHFS:
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
			m = Mount{Point: rest[:open], Kind: kindOpaque}
		}
		out = append(out, m)
	}
	return out
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
