// GitHubHost is the Host over the gh CLI.

package scm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"encoding/json"

	"github.com/edotau/claude-code/internal/textutil"
)

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]{1,64}$`)

// maxList stands in for "unlimited", which gh's --limit rejects.
const maxList = 100000

const prFields = "number,title,state,author,headRefName,baseRefName,url,headRefOid"

// runGH is the seam over the gh binary; a non-empty stdin is fed to it as the body.
var runGH = execGH

func execGH(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, ghError(err, stderr.String())
	}
	return out, nil
}

// ghError maps a gh failure to a message naming the fix; gh's account choice is the active one.
func ghError(err error, stderr string) error {
	text := strings.TrimSpace(stderr)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errors.New("the gh CLI is not installed")
	case strings.Contains(text, "rate limit"):
		return errors.New("GitHub rate limit reached; try again in a minute")
	case strings.Contains(text, "gh auth login") || strings.Contains(text, "HTTP 401"):
		return errors.New("not signed in to GitHub; run `gh auth status` and `gh auth login`")
	case strings.Contains(text, "HTTP 404") || strings.Contains(text, "Could not resolve"):
		return errors.New("repository not found, or the active gh account has no access (see `gh auth status`)")
	}
	first, _, _ := strings.Cut(text, "\n")
	return fmt.Errorf("gh: %s (%v)", strings.TrimPrefix(first, "gh: "), err) // gh api already prefixes its errors
}

// GitHubHost is a GitHub repository.
type GitHubHost struct{ Repo string }

// NewGitHub binds owner/name.
func NewGitHub(owner, name string) *GitHubHost { return &GitHubHost{Repo: owner + "/" + name} }

type ghPR struct {
	Number      int                    `json:"number"`
	Title       string                 `json:"title"`
	State       string                 `json:"state"`
	Author      struct{ Login string } `json:"author"`
	HeadRefName string                 `json:"headRefName"`
	BaseRefName string                 `json:"baseRefName"`
	URL         string                 `json:"url"`
	HeadRefOid  string                 `json:"headRefOid"`
}

func (r ghPR) pr() PR {
	return PR{Host: GitHub, ID: strconv.Itoa(r.Number), Title: r.Title, State: strings.ToLower(r.State),
		Author: r.Author.Login, Source: r.HeadRefName, Target: r.BaseRefName, URL: r.URL, HeadSHA: r.HeadRefOid}
}

func (h *GitHubHost) List(ctx context.Context, o ListOpts) ([]PR, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = maxList
	}
	args := []string{"pr", "list", "--repo", h.Repo, "--state", o.State, "--limit", strconv.Itoa(limit), "--json", prFields}
	if o.Author != "" {
		args = append(args, "--author", o.Author)
	}
	out, err := runGH(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var raws []ghPR
	if err := json.Unmarshal(out, &raws); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	prs := make([]PR, len(raws))
	for i, r := range raws {
		prs[i] = r.pr()
	}
	return prs, nil
}

func (h *GitHubHost) View(ctx context.Context, id string) (PR, error) {
	out, err := runGH(ctx, "", "pr", "view", id, "--repo", h.Repo, "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var r ghPR
	if err := json.Unmarshal(out, &r); err != nil {
		return PR{}, fmt.Errorf("gh pr view: %w", err)
	}
	return r.pr(), nil
}

func (h *GitHubHost) Diff(ctx context.Context, id string) (string, error) {
	out, err := runGH(ctx, "", "pr", "diff", id, "--repo", h.Repo)
	return string(out), err
}

func (h *GitHubHost) Create(ctx context.Context, o CreateOpts) (PR, error) {
	args := []string{"pr", "create", "--repo", h.Repo, "--title", o.Title, "--body-file", "-"}
	if o.Base != "" {
		args = append(args, "--base", o.Base)
	}
	if o.Draft {
		args = append(args, "--draft")
	}
	out, err := runGH(ctx, o.Body, args...)
	if err != nil {
		return PR{}, err
	}
	return h.View(ctx, textutil.LastLine(string(out)))
}

func (h *GitHubHost) Checks(ctx context.Context, id, sha string) ([]Check, error) {
	if id != "" {
		if err := checkDigits("PR id", id); err != nil {
			return nil, err
		}
	}
	if id == "" && !hexRe.MatchString(sha) {
		return nil, fmt.Errorf("invalid sha %q: want hex", sha)
	}
	if id != "" { // `gh pr checks --json` needs gh >= 2.51 (Ubuntu ships 2.46): read the PR head's check runs instead
		pr, err := h.View(ctx, id)
		if err != nil {
			return nil, err
		}
		if sha = pr.HeadSHA; !hexRe.MatchString(sha) {
			return nil, fmt.Errorf("PR %s has no head commit", id)
		}
	}
	out, err := runGH(ctx, "", "api", "repos/"+h.Repo+"/commits/"+sha+"/check-runs?per_page=100", "--paginate", "--jq", ".check_runs[]")
	if err != nil {
		return nil, err
	}
	var checks []Check
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var r struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		}
		if err := dec.Decode(&r); err == io.EOF {
			return checks, nil
		} else if err != nil {
			return nil, fmt.Errorf("gh api check-runs: %w", err)
		}
		checks = append(checks, Check{Name: r.Name, State: conclusionState(r.Conclusion), URL: r.HTMLURL})
	}
}

// conclusionState folds a check-run conclusion; an empty one is still running.
func conclusionState(c string) string {
	switch c {
	case "success", "neutral", "skipped":
		return "pass"
	case "":
		return "pending"
	}
	return "fail"
}

func (h *GitHubHost) Comment(ctx context.Context, id, body, replyTo string) error {
	if !digitsRe.MatchString(id) || (replyTo != "" && !digitsRe.MatchString(replyTo)) {
		return fmt.Errorf("invalid PR id %q or reply-to %q: want digits", id, replyTo)
	}
	if replyTo != "" {
		_, err := runGH(ctx, body, "api", "repos/"+h.Repo+"/pulls/"+id+"/comments/"+replyTo+"/replies", "-F", "body=@-")
		return err
	}
	_, err := runGH(ctx, body, "pr", "comment", id, "--repo", h.Repo, "--body-file", "-")
	return err
}
