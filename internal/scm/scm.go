// Package scm is the one interface over pull-request, CI and comment operations on GitHub and Bitbucket Data Center.
package scm

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// Host kinds.
const (
	GitHub    = "github"
	Bitbucket = "bitbucket"
)

// PR is one pull request in the row shape shared by every host.
type PR struct {
	Host    string  `json:"host"`
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	State   string  `json:"state"` // open|merged|declined|closed
	Author  string  `json:"author"`
	Source  string  `json:"source"`
	Target  string  `json:"target"`
	URL     string  `json:"url"`
	HeadSHA string  `json:"headSha,omitempty"`
	Checks  []Check `json:"checks,omitempty"`
}

// Check is one CI result.
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"` // pass|fail|pending
	URL   string `json:"url,omitempty"`
}

// ListOpts filters Host.List; State is open|merged|all and Author may be "@me".
type ListOpts struct {
	State, Author string
	Limit         int
}

// CreateOpts is the input of Host.Create; an empty Base means the repo's default branch.
type CreateOpts struct {
	Title, Body, Base string
	Draft             bool
}

// Host is one source host bound to a repository.
type Host interface {
	List(ctx context.Context, o ListOpts) ([]PR, error)
	View(ctx context.Context, id string) (PR, error)
	Diff(ctx context.Context, id string) (string, error)
	Create(ctx context.Context, o CreateOpts) (PR, error)
	// Checks reads a PR's CI results, or a commit's when id is empty and sha is set.
	Checks(ctx context.Context, id, sha string) ([]Check, error)
	Comment(ctx context.Context, id, body, replyTo string) error
}

// Repo names the repository a remote points at: Owner is the GitHub owner or Bitbucket project key.
type Repo struct {
	Kind, Host, Owner, Name string
}

// Detect classifies a git remote URL; any non-GitHub remote is Bitbucket.
func Detect(remote string) (Repo, error) {
	host, path, ok := splitRemote(strings.TrimSpace(remote))
	if !ok {
		return Repo{}, fmt.Errorf("cannot parse git remote %q", redact(remote))
	}
	shown := redact(remote)
	lower := strings.ToLower(host)
	isHTTP := strings.HasPrefix(strings.TrimSpace(remote), "http")
	parts := strings.FieldsFunc(strings.TrimSuffix(strings.TrimRight(path, "/"), ".git"), func(r rune) bool { return r == '/' })
	if lower == "github.com" || strings.HasSuffix(lower, ".github.com") {
		if len(parts) != 2 {
			return Repo{}, fmt.Errorf("remote %q is not owner/repo", shown)
		}
		return Repo{GitHub, host, parts[0], parts[1]}, nil
	}
	return bitbucketRepo(host, parts, isHTTP, shown)
}

// bitbucketRepo reads {project}/{repo} behind /scm/ (https) or an ssh port segment.
func bitbucketRepo(host string, parts []string, isHTTP bool, remote string) (Repo, error) {
	hasScm := false
	for i, p := range parts { // a context path (/bitbucket, /stash) may precede scm
		if strings.EqualFold(p, "scm") {
			parts, hasScm = parts[i+1:], true
			break
		}
	}
	if !hasScm && len(parts) > 0 && textutil.IsDigits(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) != 2 || (isHTTP && !hasScm) {
		return Repo{}, fmt.Errorf("remote %q is not a Bitbucket {project}/{repo} path", remote)
	}
	return Repo{Bitbucket, host, strings.ToUpper(parts[0]), parts[1]}, nil
}

// redact drops userinfo from a URL remote so credentials never reach an error message.
func redact(remote string) string {
	return userinfo.ReplaceAllString(remote, "${1}***@")
}

var userinfo = regexp.MustCompile(`(://)[^/@\s]*@`)

// splitRemote returns host and path of an https, ssh:// or scp-like remote.
func splitRemote(r string) (host, path string, ok bool) {
	if strings.Contains(r, "://") {
		u, err := url.Parse(r)
		if err != nil || u.Hostname() == "" {
			return "", "", false
		}
		return u.Hostname(), u.Path, true
	}
	at := strings.LastIndex(r, "@")
	h, p, found := strings.Cut(r[at+1:], ":")
	return h, p, found && h != "" && p != ""
}

// OriginURL reads the current directory's origin remote.
func OriginURL(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
