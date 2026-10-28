package router

import (
	"context"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// workRepoMarkers loads the configured work-repo markers (routing.opencode.workRepoMarkers): remote-URL
// substrings, a host suffix or an owner/org path segment, checked case-insensitively against every
// remote's URL. Empty markers with no error (the default) means no repo is a work repo. A registry load
// error is reported, not swallowed: IsWorkRepo fails closed on it (free blocked), never fails open.
var workRepoMarkers = func() ([]string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	return reg.Routing.Opencode.Markers(), nil
}

// gitRemoteURLs is the exec seam over `git -C dir remote -v`; tests replace it. It lists every
// remote's fetch and push URLs (both lines `remote -v` prints per remote).
var gitRemoteURLs = func(dir string) ([]string, error) {
	if dir == "" {
		dir = "."
	}
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "remote", "-v").Output()
	if err != nil {
		return nil, err
	}
	var urls []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			urls = append(urls, fields[1])
		}
	}
	return urls, nil
}

// IsWorkRepo reports whether any of dir's git remotes (fetch or push URL) name a configured work host or
// org (workRepoMarkers); "" checks the current working directory. No markers configured (and no load
// error) is personal, with no git call at all. Not a git repo, or no remotes, is personal. A markers
// load error, or a git error inside a real repo (e.g. corrupt config), fails closed as work.
func IsWorkRepo(dir string) bool {
	markers, err := workRepoMarkers()
	if err != nil {
		return true // registry unreadable: fail closed, never fail open on free
	}
	if len(markers) == 0 {
		return false
	}
	if dir == "" {
		dir = "."
	}
	if err := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "--git-dir").Run(); err != nil {
		return false // not a git repo
	}
	urls, err := gitRemoteURLs(dir)
	if err != nil {
		return true // git errored inside a repo: fail closed
	}
	for _, u := range urls {
		u = strings.ToLower(u)
		for _, m := range markers {
			if strings.Contains(u, strings.ToLower(m)) {
				return true
			}
		}
	}
	return false
}
