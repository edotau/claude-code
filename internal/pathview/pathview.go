package pathview

import (
	"errors"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// ErrNotReachable means no known mount gives the requested view; the path is never guessed.
var ErrNotReachable = errors.New("pathview: no known view of the path from that target")

// Style is the notation ViewFrom renders.
type Style int

// View styles.
const (
	StyleWindows Style = iota + 1 // C:\dir\file, falling back to a UNC form
	StyleUNC                      // \\server\share\file or \\wsl.localhost\<distro>\file
	StyleURL                      // smb://, nfs:// or file://
)

// Target is who the path is rendered for: Host "" is this machine's Windows side; a named Host is a remote
// machine, reachable only when it is the server of one of our mounts.
type Target struct {
	Style Style
	Host  string
}

// Resolver maps paths against an explicit mount list, so tests inject one.
type Resolver struct {
	Mounts     []Mount
	Distro     string                           // this WSL distro, "" off WSL
	Home       string                           // home directory for "~"
	LookupHome func(name string) (string, bool) // home of "~name"; nil uses os/user
}

// NewResolver builds a Resolver for this machine around mounts.
func NewResolver(mounts []Mount) *Resolver {
	home, _ := os.UserHomeDir()
	return &Resolver{Mounts: mounts, Distro: os.Getenv("WSL_DISTRO_NAME"), Home: home}
}

// Mounts reads this machine's live mount table.
func Mounts() []Mount { return readMounts() }

// Resolve maps p to this machine's view using the live mounts.
func Resolve(p string) (string, bool) { return Default().Resolve(p) }

// ViewFrom renders local for target using the live mounts.
func ViewFrom(local string, target Target) (string, error) { return Default().ViewFrom(local, target) }

// Resolve maps p (Windows, UNC, URL, ~, remote-host or POSIX form) to a local absolute path.
func (r *Resolver) Resolve(p string) (string, bool) {
	p = unquote(strings.TrimSpace(p))
	switch {
	case p == "":
		return "", false
	case strings.HasPrefix(p, "~"):
		return r.resolveTilde(p)
	case strings.HasPrefix(p, `\\`):
		return r.resolveUNC(strings.ReplaceAll(p, `\`, "/"))
	case isDrivePath(p):
		return r.resolveDrive(p)
	}
	if scheme, rest, ok := strings.Cut(p, "://"); ok {
		return r.resolveURL(strings.ToLower(scheme), rest)
	}
	if strings.HasPrefix(p, "//") {
		if l, ok := r.resolveUNC(p); ok {
			return l, true
		}
	}
	if strings.HasPrefix(p, "/") {
		return cleanLocal(p), true
	}
	if host, rp, ok := splitHostPath(p); ok && !isDrivePath(p) {
		return r.fromRemote(host, rp, KindNFS, KindSSHFS)
	}
	return "", false
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func isDrivePath(p string) bool {
	return len(p) >= 2 && isLetter(p[0]) && p[1] == ':' && (len(p) == 2 || p[2] == '\\' || p[2] == '/')
}

func (r *Resolver) resolveDrive(p string) (string, bool) {
	rest := path.Clean("/" + strings.ReplaceAll(p[2:], `\`, "/")) // Windows clamps ".." at the drive root
	var cands []candidate
	for i := range r.Mounts {
		m := &r.Mounts[i]
		if m.Kind != KindDrvFS || !strings.EqualFold(m.Drive, p[:1]) {
			continue
		}
		if sub, ok := trimPrefixPath(rest, m.Root, false); ok {
			cands = append(cands, candidate{m: m, rest: sub, specificity: len(m.Root)})
		}
	}
	return r.firstWithin(cands)
}

// candidate is a mount that matches a source path, with the remainder and how specific the match is.
type candidate struct {
	m           *Mount
	rest        string
	raw         string // rest with `..` kept, "" when rest has none; resolved through the mount's symlinks
	specificity int
}

// firstWithin tries candidates most specific first and returns the first one an inner mount does not obscure.
func (r *Resolver) firstWithin(cands []candidate) (string, bool) {
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].specificity > cands[j].specificity })
	for _, c := range cands {
		if local, ok := r.within(c.m, c.rest, c.raw); ok {
			return local, true
		}
	}
	return "", false
}

// within joins rest under m's point, refusing a result that an inner (opaque or other) mount covers.
func (r *Resolver) within(m *Mount, rest, raw string) (string, bool) {
	local := filepath.Join(m.Point, rest)
	if owner, _ := r.mountFor(local); owner != m {
		return "", false
	}
	if raw != "" {
		return cleanLocal(m.Point + "/" + raw), true
	}
	return cleanLocal(local), true
}

func (r *Resolver) resolveTilde(p string) (string, bool) {
	name, rest, _ := strings.Cut(p[1:], "/")
	home := r.Home
	if name != "" {
		home = r.homeOf(name)
	}
	if home == "" {
		return "", false
	}
	return cleanLocal(home + "/" + rest), true
}

func (r *Resolver) homeOf(name string) string {
	if r.LookupHome != nil {
		if h, ok := r.LookupHome(name); ok {
			return h
		}
		return ""
	}
	if u, err := user.Lookup(name); err == nil {
		return u.HomeDir
	}
	return ""
}

// resolveUNC handles "//host/share/rest" (slashes already forward).
func (r *Resolver) resolveUNC(p string) (string, bool) {
	host, rest, _ := strings.Cut(strings.TrimLeft(p, "/"), "/")
	if strings.EqualFold(host, "wsl.localhost") || strings.EqualFold(host, "wsl$") {
		distro, sub, _ := strings.Cut(rest, "/")
		if r.Distro == "" || !strings.EqualFold(distro, r.Distro) {
			return "", false
		}
		return cleanLocal("/" + sub), true
	}
	return r.fromRemote(host, rest, KindSMB)
}

func (r *Resolver) resolveURL(scheme, rest string) (string, bool) {
	switch scheme {
	case "file":
		u, err := url.Parse("file://" + rest)
		if err != nil {
			return "", false
		}
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return r.resolveUNC("//" + u.Host + u.Path)
		}
		if len(u.Path) > 2 && u.Path[0] == '/' && isDrivePath(u.Path[1:]) {
			return r.resolveDrive(u.Path[1:])
		}
		return cleanLocal(u.Path), u.Path != ""
	case "smb", "nfs":
		u, err := url.Parse(scheme + "://" + rest)
		if err != nil || u.Hostname() == "" {
			return "", false
		}
		if scheme == "smb" {
			return r.fromRemote(u.Hostname(), strings.TrimPrefix(u.Path, "/"), KindSMB)
		}
		return r.fromRemote(u.Hostname(), u.Path, KindNFS)
	}
	return "", false
}

// fromRemote maps host plus a remote path onto the most specific unobscured mount of one of kinds.
func (r *Resolver) fromRemote(host, rp string, kinds ...Kind) (string, bool) {
	var cands []candidate
	for i := range r.Mounts {
		m := &r.Mounts[i]
		if !hasKind(kinds, m.Kind) || !strings.EqualFold(m.Host, host) {
			continue
		}
		if rest, ok := trimPrefixPath(rp, m.RemotePath, m.Kind == KindSMB); ok {
			cands = append(cands, candidate{m: m, rest: rest, raw: rawRest(rp, m.RemotePath, m.Kind == KindSMB), specificity: len(m.RemotePath)})
		}
	}
	return r.firstWithin(cands)
}

// rawRest is p below prefix with `..` kept, so the local mount resolves it through symlinks; "" when p has no `..`.
func rawRest(p, prefix string, fold bool) string {
	segs := strings.Split(p, "/")
	if !slices.Contains(segs, "..") {
		return ""
	}
	var raw []string
	for _, s := range segs {
		if s != "" && s != "." {
			raw = append(raw, s)
		}
	}
	pre := strings.Split(strings.Trim(path.Clean("/"+prefix), "/"), "/")
	if pre[0] == "" {
		pre = nil
	}
	if len(raw) < len(pre) {
		return ""
	}
	for i, s := range pre {
		if raw[i] != s && !(fold && strings.EqualFold(raw[i], s)) {
			return ""
		}
	}
	rest, depth := raw[len(pre):], 0
	for _, s := range rest {
		if s == ".." {
			if depth--; depth < 0 {
				return "" // climbs out of the mount root: the lexical result decides
			}
		} else {
			depth++
		}
	}
	return strings.Join(rest, "/")
}

func hasKind(kinds []Kind, k Kind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// trimPrefixPath strips prefix from p on a path-element boundary.
func trimPrefixPath(p, prefix string, fold bool) (string, bool) {
	p, prefix = path.Clean("/"+p), path.Clean("/"+prefix)
	if fold {
		if len(p) < len(prefix) || !strings.EqualFold(p[:len(prefix)], prefix) {
			return "", false
		}
	} else if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := p[len(prefix):]
	if rest != "" && rest[0] != '/' && prefix != "/" {
		return "", false
	}
	return strings.TrimPrefix(rest, "/"), true
}

// ViewFrom renders the local path for target; ErrNotReachable when no known mount supports it.
func (r *Resolver) ViewFrom(local string, target Target) (string, error) {
	local = cleanLocal(local) // keeps `..` through symlinks, like Resolve
	if !filepath.IsAbs(local) {
		return "", ErrNotReachable
	}
	m, rest := r.mountFor(local)
	if target.Host != "" {
		if m == nil || !isRemote(m.Kind) || !strings.EqualFold(m.Host, target.Host) {
			return "", ErrNotReachable
		}
		return r.render(m, rest, local, target.Style)
	}
	switch target.Style {
	case StyleWindows:
		if m != nil && m.Kind == KindDrvFS {
			return m.Drive + `:\` + strings.ReplaceAll(strings.TrimPrefix(path.Join(m.Root, rest), "/"), "/", `\`), nil
		}
		return r.render(m, rest, local, StyleUNC)
	case StyleUNC, StyleURL:
		return r.render(m, rest, local, target.Style)
	}
	return "", ErrNotReachable
}

func isRemote(k Kind) bool { return k == KindSMB || k == KindNFS || k == KindSSHFS }

// mountFor is the longest classified mount containing local, and local relative to it.
func (r *Resolver) mountFor(local string) (*Mount, string) {
	var best *Mount
	var bestRest string
	for i := range r.Mounts {
		m := &r.Mounts[i]
		if rest, ok := trimPrefixPath(local, m.Point, false); ok && (best == nil || len(m.Point) >= len(best.Point)) {
			best, bestRest = m, rest
		}
	}
	return best, bestRest
}

func (r *Resolver) render(m *Mount, rest, local string, style Style) (string, error) {
	switch {
	case m != nil && m.Kind == KindSMB:
		if style == StyleURL {
			return (&url.URL{Scheme: "smb", Host: m.Host, Path: "/" + path.Join(m.RemotePath, rest)}).String(), nil
		}
		return `\\` + m.Host + `\` + strings.ReplaceAll(path.Join(m.RemotePath, rest), "/", `\`), nil
	case m != nil && m.Kind == KindNFS && style == StyleURL:
		return (&url.URL{Scheme: "nfs", Host: m.Host, Path: path.Join(m.RemotePath, rest)}).String(), nil
	case m != nil && m.Kind == KindSSHFS, m != nil && m.Kind == KindNFS:
		return "", ErrNotReachable
	case style == StyleURL:
		if m != nil && m.Kind == KindDrvFS {
			return (&url.URL{Scheme: "file", Path: "/" + m.Drive + ":/" + strings.TrimPrefix(path.Join(m.Root, rest), "/")}).String(), nil
		}
		return (&url.URL{Scheme: "file", Path: local}).String(), nil
	case r.Distro != "":
		return `\\wsl.localhost\` + r.Distro + strings.ReplaceAll(local, "/", `\`), nil
	}
	return "", ErrNotReachable
}

// cleanLocal cleans p lexically, except a ".." path is resolved through symlinks (existing paths) so
// /dev/fd/../status keeps its meaning; a path that does not exist falls back to the lexical result.
func cleanLocal(p string) string {
	if slices.Contains(strings.Split(p, "/"), "..") {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
	}
	return filepath.Clean(p)
}
