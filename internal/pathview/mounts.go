// Package pathview maps paths between this machine's view and Windows, UNC and share views, driven by the live mount table.
package pathview

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Kind classifies a mount by how its content is reached.
type Kind int

// Mount kinds.
const (
	KindOpaque Kind = iota // any other mount; kept so it fences off the mounts beneath it
	KindDrvFS              // WSL /mnt/<drive>, a Windows drive
	KindSMB                // //server/share (cifs, smbfs)
	KindNFS                // host:/export
	KindSSHFS              // user@host:path
	KindVolume             // other macOS /Volumes/<name> share
)

// Mount is one classified mount-table entry.
type Mount struct {
	Point      string // local mount point
	Source     string // the remote source as the kernel reports it
	FSType     string
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

// classify turns a raw mount entry into a Mount; ok is false for filesystems that are not drvfs/remote/Volumes.
func classify(point, source, fstype string) (Mount, bool) {
	m := Mount{Point: point, Source: source, FSType: fstype}
	switch {
	case isDriveSource(source):
		m.Kind, m.Drive = KindDrvFS, strings.ToUpper(source[:1])
	case isSMB(source, fstype):
		host, share, ok := splitSMB(source)
		if !ok {
			return Mount{}, false
		}
		m.Kind, m.Host, m.RemotePath = KindSMB, host, share
	case strings.HasPrefix(fstype, "nfs"):
		host, p, ok := splitHostPath(source)
		if !ok {
			return Mount{}, false
		}
		m.Kind, m.Host, m.RemotePath = KindNFS, host, p
	case strings.Contains(fstype, "sshfs") || (strings.Contains(fstype, "fuse") && strings.Contains(source, "@")):
		host, p, ok := splitHostPath(source)
		if !ok {
			return Mount{}, false
		}
		m.Kind, m.Host, m.RemotePath = KindSSHFS, host, p
	case strings.HasPrefix(point, "/Volumes/") && !strings.HasPrefix(source, "/dev/"):
		m.Kind = KindVolume
	default:
		return Mount{}, false
	}
	return m, true
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
	return strings.HasPrefix(source, "//") && fstype != "9p"
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
	h, p, found := strings.Cut(source, ":")
	if strings.HasPrefix(source, "[") {
		if end := strings.Index(source, "]:"); end > 0 {
			h, p, found = source[1:end], source[end+2:], true
		}
	}
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	if !found || h == "" || !strings.HasPrefix(p, "/") {
		return "", "", false
	}
	return h, p, true
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
	if len(parts) < 2 || !(strings.EqualFold(parts[0], "wsl.localhost") || strings.EqualFold(parts[0], "wsl$")) {
		return ""
	}
	return parts[1]
}
