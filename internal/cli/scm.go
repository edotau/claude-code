package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/scm"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

var page = helptext.Page{
	Usage: "claude-code scm <pr list|view|diff|create | checks | comment> [flags]",
	About: "Pull requests, CI checks and comments for the origin remote: GitHub via gh, " +
		"Bitbucket Data Center via REST (BITBUCKET_URL in vault env, default https://<remote host>; BITBUCKET_TOKEN in vault).",
	Commands: [][2]string{
		{"pr", "list (--state, --author, --limit) | view <id> | diff <id> | create"},
		{"checks", "CI results of a PR <id>, or of --sha"},
		{"comment", "comment on a PR <id> (--body-file, --reply-to)"},
	},
}

// open resolves the origin remote to a Host; tests swap it.
var open = openOrigin

func openOrigin(ctx context.Context) (scm.Host, error) {
	remote, err := scm.OriginURL(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := scm.Detect(remote)
	if err != nil {
		return nil, err
	}
	if repo.Kind == scm.GitHub {
		return scm.NewGitHub(repo.Owner, repo.Name), nil
	}
	token, _, err := vault.Lookup("BITBUCKET_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("BITBUCKET_TOKEN: %w", err)
	}
	if token == "" {
		return nil, errors.New("BITBUCKET_TOKEN is not set; run `claude-code vault set BITBUCKET_TOKEN`")
	}
	base := bitbucketURL()
	if base == "" { // the ssh port (7999) is not the HTTP port, so the default is the bare host
		base = "https://" + repo.Host
	}
	return scm.NewBitbucket(base, repo.Owner, repo.Name, token), nil
}

// bitbucketURL is the Data Center server root from vault env, then the process env.
func bitbucketURL() string {
	if v := strings.TrimSpace(providers.CustomEnv()["BITBUCKET_URL"]); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("BITBUCKET_URL"))
}

// scmUsage prints the page to stderr: exit 2 when nothing was asked for, 0 for -h.
func scmUsage(bare bool) int {
	page.Fprint(os.Stderr)
	if bare {
		return 2
	}
	return 0
}

// cmdScm is the `scm` verb.
func cmdScm(args []string) int {
	if len(args) == 0 || helptext.WantsHelp(args[:1]) {
		return scmUsage(len(args) == 0)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "pr":
		if len(rest) == 0 || helptext.WantsHelp(rest[:1]) {
			return scmUsage(len(rest) == 0)
		}
		return dispatch(rest[0], rest[1:])
	case "checks", "comment":
		return dispatch(verb, rest)
	}
	return helptext.FailUsage("scm: unknown command %q (see claude-code scm -h)", verb)
}

type opts struct {
	format, state, author, title, bodyFile, base, sha, replyTo *string
	limit                                                      *int
	draft                                                      *bool
}

func newFlags(name string) (*flag.FlagSet, *opts) {
	fs := helptext.NewFlagSet("scm "+name, helptext.Page{Usage: "claude-code scm " + name + " [flags]"})
	o := &opts{format: helptext.FormatFlag(fs)}
	o.state = fs.String("state", "open", "list: open|merged|all")
	o.author = fs.String("author", "", "list: author, or @me")
	o.limit = fs.Int("limit", 30, "list: maximum rows")
	o.title = fs.String("title", "", "create: PR title")
	o.bodyFile = fs.String("body-file", "", "create, comment: file holding the body")
	o.base = fs.String("base", "", "create: target branch (default: repo default)")
	o.draft = fs.Bool("draft", false, "create: open as a draft")
	o.sha = fs.String("sha", "", "checks: commit instead of a PR")
	o.replyTo = fs.String("reply-to", "", "comment: parent comment id")
	return fs, o
}

func dispatch(name string, args []string) int {
	fs, o := newFlags(name)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*o.format); err != nil {
		return helptext.BadFormat("scm", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if msg := checkArgs(name, pos, o); msg != "" {
		return helptext.FailUsage("scm %s: %s", name, msg)
	}
	h, err := open(ctx)
	if err != nil {
		return helptext.Fail("scm %s: %v", name, err)
	}
	if err := runVerb(ctx, h, name, pos, o); err != nil {
		return helptext.Fail("scm %s: %v", name, err)
	}
	return 0
}

// checkArgs validates positionals and required flags before any host is opened.
func checkArgs(name string, pos []string, o *opts) string {
	switch name {
	case "list":
		if *o.state != "open" && *o.state != "merged" && *o.state != "all" {
			return "--state wants open, merged or all"
		}
	case "view", "diff", "comment":
		if len(pos) != 1 {
			return "want exactly one PR id"
		}
		if name == "comment" && *o.bodyFile == "" {
			return "--body-file is required"
		}
	case "create":
		if *o.title == "" {
			return "--title is required"
		}
	case "checks":
		if len(pos) > 1 || (len(pos) == 0) == (*o.sha == "") {
			return "want one PR id or --sha"
		}
	default:
		return "unknown command"
	}
	return ""
}

func readBody(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func runVerb(ctx context.Context, h scm.Host, name string, pos []string, o *opts) error {
	json := *o.format == helptext.JSON
	switch name {
	case "list":
		prs, err := h.List(ctx, scm.ListOpts{State: *o.state, Author: *o.author, Limit: *o.limit})
		return emit(json, prs, err, func() {
			for _, p := range prs {
				fmt.Printf("#%s\t%s\t%s\t%s->%s\t%s\n", p.ID, p.State, p.Author, p.Source, p.Target, p.Title)
			}
		})
	case "view":
		p, err := h.View(ctx, pos[0])
		return emit(json, p, err, func() { fmt.Printf("#%s %s [%s] %s->%s\n%s\n", p.ID, p.Title, p.State, p.Source, p.Target, p.URL) })
	case "diff":
		d, err := h.Diff(ctx, pos[0])
		if err == nil {
			fmt.Print(d)
		}
		return err
	case "create":
		body, err := readBody(*o.bodyFile)
		if err != nil {
			return err
		}
		p, err := h.Create(ctx, scm.CreateOpts{Title: *o.title, Body: body, Base: *o.base, Draft: *o.draft})
		return emit(json, p, err, func() { fmt.Println(p.URL) })
	case "checks":
		id := ""
		if len(pos) == 1 {
			id = pos[0]
		}
		cs, err := h.Checks(ctx, id, *o.sha)
		return emit(json, cs, err, func() {
			for _, c := range cs {
				fmt.Printf("%s\t%s\t%s\n", c.State, c.Name, c.URL)
			}
		})
	}
	body, err := readBody(*o.bodyFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("--body-file %s is empty", *o.bodyFile)
	}
	return h.Comment(ctx, pos[0], body, *o.replyTo)
}

// emit prints v as JSON or runs the text printer; a host error passes through.
func emit(asJSON bool, v any, err error, text func()) error {
	if err != nil {
		return err
	}
	if !asJSON {
		text()
		return nil
	}
	return helptext.WriteJSON(os.Stdout, v)
}

// scmSpec is the `scm` completion spec.
func scmSpec() *helptext.Node {
	leaf := func(flags ...helptext.Flag) *helptext.Node {
		return &helptext.Node{Flags: append(flags, helptext.CompletionFormatFlag())}
	}
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"pr": {Sub: map[string]*helptext.Node{
			"list":   leaf(helptext.Plain("state"), helptext.Plain("author"), helptext.Plain("limit")),
			"view":   leaf(),
			"diff":   leaf(),
			"create": leaf(helptext.Plain("title"), helptext.Plain("body-file"), helptext.Plain("base"), helptext.BoolFlag("draft")),
		}},
		"checks":  leaf(helptext.Plain("sha")),
		"comment": leaf(helptext.Plain("body-file"), helptext.Plain("reply-to")),
	}}
}
