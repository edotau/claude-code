package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// nativeLinkTarget resolves relative symlink text even when its destination no longer exists.
func nativeLinkTarget(link string) string {
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return resolvePath(target)
}

// nativeLinkTargets uses recorded paths because project slugs cannot be decoded unambiguously.
func nativeLinkTargets(root string) map[string]string {
	targets := map[string]string{}
	add := func(cwd, target string) {
		if cwd == "" || target == "" {
			return
		}
		slug := ProjectSlug(cwd)
		if prior, ok := targets[slug]; ok && prior != target {
			targets[slug] = ""
			return
		}
		targets[slug] = target
	}
	if target, err := RepoDir(root); root != "" && err == nil {
		add(root, target)
	}
	add(paths.ConfigDir(), HarnessStoreDir())
	add(StoreDir(), HomeBankDir())
	if home, err := os.UserHomeDir(); err == nil {
		add(home, HomeBankDir())
	}
	entries, _ := RepoDirs()
	for _, e := range entries {
		add(e.Dir, e.Dir)
		add(e.Origin.Origin, e.Dir)
		for _, alias := range e.Origin.Aliases {
			add(alias, e.Dir)
		}
	}
	return targets
}

// brokenNativeLinks reports only dangling links and links targeting the store root.
func brokenNativeLinks(root string) []LinkResult {
	targets := nativeLinkTargets(root)
	matches, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "memory"))
	var out []LinkResult
	for _, link := range matches {
		target := nativeLinkTarget(link)
		if target == "" {
			continue
		}
		_, err := os.Stat(link)
		if !errors.Is(err, fs.ErrNotExist) && !paths.SamePath(target, StoreDir()) {
			continue
		}
		r := LinkResult{Link: link, Action: "blocked", Detail: "cwd cannot be derived from recorded paths"}
		if r.Target = targets[filepath.Base(filepath.Dir(link))]; r.Target != "" {
			if fi, statErr := os.Stat(r.Target); statErr != nil || !fi.IsDir() {
				r.Detail = "resolved store directory is unavailable"
			} else {
				r.Action, r.Detail = "repairable", "invalid native memory target: "+target
			}
		}
		out = append(out, r)
	}
	return out
}

// repairNativeLinks leaves unresolved slugs alone for memory review to report.
func repairNativeLinks(root string) {
	for _, r := range brokenNativeLinks(root) {
		if r.Action == "repairable" {
			ensureLink(r.Link, r.Target)
		}
	}
}

func nativeLinkProblems(root string) []string {
	var problems []string
	for _, r := range brokenNativeLinks(root) {
		problems = append(problems, "native memory link "+r.Link+": "+r.Detail)
	}
	return problems
}
