// Package bitbucket is the scm.Host over the Bitbucket Data Center REST API (1.0).
package bitbucket

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/scm"
)

const authHint = "Bitbucket rejected the token; run `claude-code vault set BITBUCKET_TOKEN`"

const forbiddenHint = "Bitbucket denied access: the token lacks permission on this repository"

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// checkDigits rejects a PR or comment id that is not all digits, matching the GitHub host.
func checkDigits(what, v string) error {
	if !digitsRe.MatchString(v) {
		return fmt.Errorf("invalid %s %q: want digits", what, v)
	}
	return nil
}

// Host is one Data Center repository; the token is only ever sent as a bearer header.
type Host struct {
	Base, Project, Repo, Token string
	HTTP                       *http.Client
}

// New binds a server root (a trailing /rest/api/... is tolerated) and a repository.
func New(baseURL, project, repo, token string) *Host {
	root := regexp.MustCompile(`(?i)/rest(/api/[^/]+)?$`).ReplaceAllString(strings.TrimRight(baseURL, "/"), "")
	return &Host{Base: root, Project: project, Repo: repo, Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (h *Host) repoPath() string {
	return "/rest/api/1.0/projects/" + url.PathEscape(h.Project) + "/repos/" + url.PathEscape(h.Repo)
}

// do sends one request and returns the body; 401/403 name the vault fix without echoing the token.
func (h *Host) do(ctx context.Context, method, path string, q url.Values, body any, accept string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := h.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket %s %s: %s", method, path, strings.ReplaceAll(err.Error(), h.Token, "***"))
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(resp.Body)
	if err := statusErr(method, path, resp.StatusCode); err != nil {
		return nil, err
	}
	if rerr != nil {
		return nil, fmt.Errorf("bitbucket %s %s: reading body: %s", method, path, strings.ReplaceAll(rerr.Error(), h.Token, "***"))
	}
	return data, nil
}

// statusErr maps a non-2xx status to an error; 401 and 403 name different fixes.
func statusErr(method, path string, code int) error {
	switch {
	case code == http.StatusUnauthorized:
		return fmt.Errorf("%s (HTTP %d)", authHint, code)
	case code == http.StatusForbidden:
		return fmt.Errorf("%s (HTTP %d)", forbiddenHint, code)
	case code >= http.StatusMultipleChoices:
		return fmt.Errorf("bitbucket %s %s: HTTP %d", method, path, code)
	}
	return nil
}

func (h *Host) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	data, err := h.do(ctx, http.MethodGet, path, q, nil, "application/json")
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

type user struct {
	Name, DisplayName, Slug string
}

type ref struct {
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
	Repository   struct {
		Slug    string
		Project struct{ Key string }
	}
}

type rawPR struct {
	ID      int                                    `json:"id"`
	Title   string                                 `json:"title"`
	State   string                                 `json:"state"`
	Author  struct{ User user }                    `json:"author"`
	FromRef ref                                    `json:"fromRef"`
	ToRef   ref                                    `json:"toRef"`
	Links   struct{ Self []struct{ Href string } } `json:"links"`
}

func (r rawPR) pr() scm.PR {
	p := scm.PR{Host: scm.Bitbucket, ID: strconv.Itoa(r.ID), Title: r.Title, State: prState(r.State),
		Author: cmp.Or(r.Author.User.DisplayName, r.Author.User.Name), Source: r.FromRef.DisplayID,
		Target: r.ToRef.DisplayID, HeadSHA: r.FromRef.LatestCommit}
	if len(r.Links.Self) > 0 {
		p.URL = r.Links.Self[0].Href
	}
	return p
}

func prState(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "MERGED":
		return "merged"
	case "DECLINED", "SUPERSEDED":
		return "declined"
	}
	return "open"
}

// pages reads every page of a paged list into raws until limit rows (0 = all).
func (h *Host) pages(ctx context.Context, path string, q url.Values, limit int) ([]rawPR, error) {
	var all []rawPR
	for start := 0; ; {
		q.Set("start", strconv.Itoa(start))
		var page struct {
			Values        []rawPR
			IsLastPage    bool
			NextPageStart int
		}
		if err := h.getJSON(ctx, path, q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Values...)
		if page.IsLastPage || len(page.Values) == 0 || (limit > 0 && len(all) >= limit) {
			return all, nil
		}
		start = page.NextPageStart
	}
}

func (h *Host) List(ctx context.Context, o scm.ListOpts) ([]scm.PR, error) {
	state := strings.ToUpper(o.State)
	path, q := h.repoPath()+"/pull-requests", url.Values{"state": {state}, "limit": {"50"}}
	if o.Author == "@me" {
		path, q = "/rest/api/1.0/dashboard/pull-requests", url.Values{"role": {"AUTHOR"}, "state": {state}, "limit": {"50"}}
	}
	limit := 0
	if o.Author == "" {
		limit = o.Limit // no post-fetch filter drops rows, so the first Limit rows suffice
	}
	raws, err := h.pages(ctx, path, q, limit)
	if err != nil {
		return nil, err
	}
	prs := []scm.PR{}
	for _, r := range raws {
		if o.Author == "@me" && !h.inRepo(r.ToRef) {
			continue
		}
		if o.Author != "" && o.Author != "@me" && !strings.EqualFold(o.Author, r.Author.User.Name) && !strings.EqualFold(o.Author, r.Author.User.Slug) {
			continue
		}
		prs = append(prs, r.pr())
		if o.Limit > 0 && len(prs) >= o.Limit {
			break
		}
	}
	return prs, nil
}

func (h *Host) inRepo(r ref) bool {
	return strings.EqualFold(r.Repository.Slug, h.Repo) && strings.EqualFold(r.Repository.Project.Key, h.Project)
}

func (h *Host) View(ctx context.Context, id string) (scm.PR, error) {
	if err := checkDigits("PR id", id); err != nil {
		return scm.PR{}, err
	}
	var r rawPR
	if err := h.getJSON(ctx, h.repoPath()+"/pull-requests/"+url.PathEscape(id), nil, &r); err != nil {
		return scm.PR{}, err
	}
	return r.pr(), nil
}

func (h *Host) Diff(ctx context.Context, id string) (string, error) {
	if err := checkDigits("PR id", id); err != nil {
		return "", err
	}
	data, err := h.do(ctx, http.MethodGet, h.repoPath()+"/pull-requests/"+url.PathEscape(id)+".diff", nil, nil, "text/plain")
	return string(data), err
}

func (h *Host) Create(ctx context.Context, o scm.CreateOpts) (scm.PR, error) {
	src, err := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return scm.PR{}, fmt.Errorf("current branch: %w", err)
	}
	if strings.TrimSpace(string(src)) == "HEAD" {
		return scm.PR{}, fmt.Errorf("detached HEAD: check out and push a branch before creating a PR")
	}
	base := o.Base
	if base == "" {
		var def struct {
			DisplayID string `json:"displayId"`
		}
		if err := h.getJSON(ctx, h.repoPath()+"/default-branch", nil, &def); err != nil {
			return scm.PR{}, err
		}
		base = def.DisplayID
	}
	repo := map[string]any{"slug": h.Repo, "project": map[string]string{"key": h.Project}}
	body := map[string]any{"title": o.Title, "description": o.Body, "draft": o.Draft,
		"fromRef": map[string]any{"id": "refs/heads/" + strings.TrimSpace(string(src)), "repository": repo},
		"toRef":   map[string]any{"id": "refs/heads/" + base, "repository": repo}}
	data, err := h.do(ctx, http.MethodPost, h.repoPath()+"/pull-requests", nil, body, "application/json")
	if err != nil {
		return scm.PR{}, err
	}
	var r rawPR
	if err := json.Unmarshal(data, &r); err != nil {
		return scm.PR{}, err
	}
	return r.pr(), nil
}

func (h *Host) Checks(ctx context.Context, id, sha string) ([]scm.Check, error) {
	if id != "" {
		pr, err := h.View(ctx, id)
		if err != nil {
			return nil, err
		}
		sha = pr.HeadSHA
	}
	if sha == "" {
		return nil, fmt.Errorf("no commit to check: PR %q has no head commit", id)
	}
	checks := []scm.Check{}
	for start := 0; ; {
		var page struct {
			Values        []struct{ State, Name, Key, URL string }
			IsLastPage    bool
			NextPageStart int
		}
		q := url.Values{"limit": {"100"}, "start": {strconv.Itoa(start)}}
		if err := h.getJSON(ctx, "/rest/build-status/1.0/commits/"+url.PathEscape(sha), q, &page); err != nil {
			return nil, err
		}
		for _, v := range page.Values {
			checks = append(checks, scm.Check{Name: cmp.Or(v.Name, v.Key), State: buildState(v.State), URL: v.URL})
		}
		if page.IsLastPage || len(page.Values) == 0 {
			break
		}
		start = page.NextPageStart
	}
	return checks, nil
}

func buildState(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SUCCESSFUL":
		return "pass"
	case "FAILED", "STOPPED", "ERROR":
		return "fail"
	}
	return "pending"
}

func (h *Host) Comment(ctx context.Context, id, body, replyTo string) error {
	if err := checkDigits("PR id", id); err != nil {
		return err
	}
	payload := map[string]any{"text": body}
	if replyTo != "" {
		if err := checkDigits("reply-to", replyTo); err != nil {
			return err
		}
		parent, _ := strconv.Atoi(replyTo)
		payload["parent"] = map[string]int{"id": parent}
	}
	_, err := h.do(ctx, http.MethodPost, h.repoPath()+"/pull-requests/"+url.PathEscape(id)+"/comments", nil, payload, "application/json")
	return err
}
