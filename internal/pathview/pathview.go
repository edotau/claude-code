package pathview

import (
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

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

// Resolve maps p to this machine's view using the live mounts.
func Resolve(p string) (string, bool) { return Default().Resolve(p) }

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
	return r.resolveForm(p)
}

// resolveForm handles the non-prefix forms of p: URL, //host UNC, POSIX absolute and host:path.
func (r *Resolver) resolveForm(p string) (string, bool) {
	if scheme, rest, ok := strings.Cut(p, "://"); ok {
		return r.resolveURL(strings.ToLower(scheme), rest)
	}
	if strings.HasPrefix(p, "//") {
		return r.resolveUNC(p)
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
		resolved := cleanLocal(m.Point + "/" + raw)
		if owner, _ := r.mountFor(resolved); owner != m {
			return "", false
		}
		return resolved, true
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
		return r.resolveFileURL(rest)
	case "smb", "nfs":
		return r.resolveRemoteURL(scheme, rest)
	}
	return "", false
}

func (r *Resolver) resolveFileURL(rest string) (string, bool) {
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
}

func (r *Resolver) resolveRemoteURL(scheme, rest string) (string, bool) {
	u, err := url.Parse(scheme + "://" + rest)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	if scheme == "smb" {
		return r.fromRemote(u.Hostname(), strings.TrimPrefix(u.Path, "/"), KindSMB)
	}
	return r.fromRemote(u.Hostname(), u.Path, KindNFS)
}

// fromRemote maps host plus a remote path onto the most specific unobscured mount of one of kinds.
func (r *Resolver) fromRemote(host, rp string, kinds ...Kind) (string, bool) {
	var cands []candidate
	for i := range r.Mounts {
		m := &r.Mounts[i]
		if !slices.Contains(kinds, m.Kind) || !strings.EqualFold(m.Host, host) {
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
	if len(raw) < len(pre) || !segsHavePrefix(raw, pre, fold) {
		return ""
	}
	rest := raw[len(pre):]
	if climbsOut(rest) {
		return "" // climbs out of the mount root: the lexical result decides
	}
	return strings.Join(rest, "/")
}

// segsHavePrefix reports whether raw starts with the segments pre (case-folded when fold).
func segsHavePrefix(raw, pre []string, fold bool) bool {
	for i, s := range pre {
		if raw[i] != s && (!fold || !strings.EqualFold(raw[i], s)) {
			return false
		}
	}
	return true
}

// climbsOut reports whether the `..` segments in rest rise above its own root.
func climbsOut(rest []string) bool {
	depth := 0
	for _, s := range rest {
		if s != ".." {
			depth++
		} else if depth--; depth < 0 {
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

// WindowsPath renders local as a Windows drive path (C:\dir\file); false when no drvfs mount holds it.
func (r *Resolver) WindowsPath(local string) (string, bool) {
	local = cleanLocal(local) // keeps `..` through symlinks, like Resolve
	m, rest := r.mountFor(local)
	if !filepath.IsAbs(local) || m == nil || m.Kind != KindDrvFS {
		return "", false
	}
	return m.Drive + `:\` + strings.ReplaceAll(strings.TrimPrefix(path.Join(m.Root, rest), "/"), "/", `\`), true
}

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
