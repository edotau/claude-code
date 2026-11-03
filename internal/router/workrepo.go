package router

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// gitTimeout bounds each git call so a hung one fails closed instead of blocking a pick.
const gitTimeout = 10 * time.Second

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
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "-v").Output()
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

// gitInRepo runs `git rev-parse --git-dir`: only git's "not a git repository" verdict is false; other failures error.
func gitInRepo(dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-dir")
	cmd.Env = append(os.Environ(), "LC_ALL=C") // pin git's message language for the stderr match
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(stderr.String(), "not a git repository") {
		return false, nil
	}
	return false, err
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
	inRepo, err := gitInRepo(dir)
	if err != nil {
		return true // git missing or errored (e.g. corrupt config): fail closed
	}
	if !inRepo {
		return false
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
