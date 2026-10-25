package route

import (
	"context"
	"os/exec"
	"strings"
)

// WorkRepoMarkers are the remote-URL substrings that mark a repo as work: a host suffix or an owner/org
// path segment, checked case-insensitively against every remote's URL. One list — widen it here,
// nowhere else. Free opencode endpoints (the Zen catalog) never see a work-repo prompt.
var WorkRepoMarkers = []string{"biscrum.com", "boehringer"}

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

// IsWorkRepo reports whether any of dir's git remotes (fetch or push URL) name a work host or org
// (WorkRepoMarkers); "" checks the current working directory. Not a git repo, or no remotes, is
// personal. A git error inside a real repo (e.g. corrupt config) fails closed as work.
func IsWorkRepo(dir string) bool {
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
		for _, m := range WorkRepoMarkers {
			if strings.Contains(u, strings.ToLower(m)) {
				return true
			}
		}
	}
	return false
}
