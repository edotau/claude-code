package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// LinkResult is one projects/<slug>/memory link the harness maintains (Appendix D: native follows the symlink).
type LinkResult struct {
	Link, Target string
	Action       string // linked | ok | relinked | blocked
	Detail       string
}

// projectsMemory is Claude Code's native memory dir for a project slug.
func projectsMemory(slug string) string {
	return filepath.Join(paths.ConfigDir(), "projects", slug, "memory")
}

// LinkNativeFor repairs the native link for one session cwd, keyed the way Claude Code names the project.
func LinkNativeFor(cwd string) (LinkResult, bool) {
	if cwd == "" {
		return LinkResult{}, false
	}
	root := filepath.Clean(cwd)
	if top := mainCheckout(root); top != "" {
		root = top
	}
	target := Dir(cwd)
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return LinkResult{}, false // no store yet: never mint an origin-less dir that would squat the repo's key
	}
	return ensureLink(projectsMemory(ProjectSlug(root)), target), true
}

// ensureLink makes link a symlink to target: absent or empty dir → linked, wrong link → relinked, content → blocked.
func ensureLink(link, target string) LinkResult {
	r := LinkResult{Link: link, Target: target}
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Action = "linked"
	case err != nil:
		r.Action, r.Detail = "blocked", err.Error()
		return r
	case fi.Mode()&fs.ModeSymlink != 0:
		cur, _ := os.Readlink(link)
		if paths.SamePath(cur, target) {
			r.Action = "ok"
			return r
		}
		if err := os.Remove(link); err != nil {
			r.Action, r.Detail = "blocked", err.Error()
			return r
		}
		r.Action, r.Detail = "relinked", "was -> "+cur
	case fi.IsDir():
		if ents, err := os.ReadDir(link); err != nil || len(ents) > 0 {
			r.Action, r.Detail = "blocked", "real native dir with content: merge it into the store dir by hand"
			return r
		}
		if err := os.Remove(link); err != nil {
			r.Action, r.Detail = "blocked", err.Error()
			return r
		}
		r.Action = "linked"
	default:
		r.Action, r.Detail = "blocked", "not a directory"
		return r
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		r.Action, r.Detail = "blocked", err.Error()
		return r
	}
	if err := os.Symlink(target, link); err != nil {
		r.Action, r.Detail = "blocked", err.Error()
	}
	return r
}
