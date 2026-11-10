package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// linkResult is one projects/<slug>/memory link the harness maintains (Appendix D: native follows the symlink).
type linkResult struct {
	Link, Target string
	Action       string // linked | ok | relinked | blocked
	Detail       string
}

// projectsMemory is Claude Code's native memory dir for a project slug.
func projectsMemory(slug string) string {
	return filepath.Join(paths.ConfigDir(), "projects", slug, "memory")
}

// LinkNativeFor repairs the native link for one session cwd, keyed the way Claude Code names the project.
func LinkNativeFor(cwd string) (linkResult, bool) {
	if cwd == "" {
		return linkResult{}, false
	}
	root := filepath.Clean(cwd)
	if top := paths.MainCheckout(root, gitOut); top != "" {
		root = paths.CanonDeep(top)
	}
	target, err := RepoDir(cwd)
	if err != nil {
		return linkResult{}, false
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return linkResult{}, false // no store yet: never mint an origin-less dir that would squat the repo's key
	}
	repairNativeLinks(cwd)
	return ensureLink(projectsMemory(paths.ProjectSlug(root)), target), true
}

// ensureLink makes link a symlink to target: absent or empty dir → linked, wrong link → relinked, content → blocked.
func ensureLink(link, target string) linkResult {
	r := linkResult{Link: link, Target: target}
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Action = "linked"
	case err != nil:
		return r.blocked(err.Error())
	case fi.Mode()&fs.ModeSymlink != 0:
		if r = relink(r); r.Action != "relinked" {
			return r
		}
	case fi.IsDir():
		if r = clearEmptyDir(r); r.Action != "linked" {
			return r
		}
	default:
		return r.blocked("not a directory")
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return r.blocked(err.Error())
	}
	if err := os.Symlink(target, link); err != nil {
		r.Action, r.Detail = "blocked", err.Error()
	}
	return r
}

// blocked returns r marked blocked with detail.
func (r linkResult) blocked(detail string) linkResult {
	r.Action, r.Detail = "blocked", detail
	return r
}

// relink removes a wrong symlink (Action "relinked"), keeps a right one (Action "ok"), or reports blocked.
func relink(r linkResult) linkResult {
	cur, _ := os.Readlink(r.Link)
	if paths.SamePath(nativeLinkTarget(r.Link), r.Target) {
		r.Action = "ok"
		return r
	}
	if err := os.Remove(r.Link); err != nil {
		return r.blocked(err.Error())
	}
	r.Action, r.Detail = "relinked", "was -> "+cur
	return r
}

// clearEmptyDir removes an empty real dir (Action "linked"); a dir with content is blocked.
func clearEmptyDir(r linkResult) linkResult {
	if ents, err := os.ReadDir(r.Link); err != nil || len(ents) > 0 {
		return r.blocked("real native dir with content: merge it into the store dir by hand")
	}
	if err := os.Remove(r.Link); err != nil {
		return r.blocked(err.Error())
	}
	r.Action = "linked"
	return r
}
