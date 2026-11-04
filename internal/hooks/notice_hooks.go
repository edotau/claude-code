package hooks

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/notices"
	"github.com/edotau/claude-code/internal/paths"
)

// gitAnyCmd pre-screens a Bash command for any git invocation: the moment a drained notice matters most.
var gitAnyCmd = regexp.MustCompile(`\bgit\b`)

// noticeContext drains this session's inbox and adds the divergence line; warn names an infra error (fail open).
func noticeContext(ctx context.Context, in *Input) (text, warn string) {
	var lines []string
	for _, n := range notices.Drain(in.SessionID) {
		lines = append(lines, fmt.Sprintf("notice (%s, from %s): %s", n.Kind, n.From, n.Text))
	}
	line, err := divergedLine(ctx, in)
	if err != nil {
		warn = fmt.Sprintf("notices: WARNING — could not check branch divergence (%v); skipped", err)
	}
	if line != "" {
		lines = append(lines, line)
	}
	return capContext(strings.Join(lines, "\n")), warn
}

// divergence is a checkout whose HEAD and upstream both have commits the other lacks.
type divergence struct {
	top, head, up, branch, upName string
	ahead, behind                 int
}

// divergedLine names a shared checkout's local/upstream divergence once per (HEAD, upstream) pair; "" otherwise.
func divergedLine(ctx context.Context, in *Input) (string, error) {
	dir := in.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	d, err := divergedAt(ctx, dir)
	if d == nil || err != nil {
		return "", err
	}
	shared, err := checkoutShared(ctx, dir)
	if !shared || err != nil {
		return "", err
	}
	if !firstSighting(in.SessionID, "diverged\t"+d.head+"\t"+d.up) {
		return "", nil
	}
	return fmt.Sprintf("local %s diverged from %s (ahead %d, behind %d): a commit was rewritten; do not commit until "+
		"it is reconciled%s", d.branch, d.upName, d.ahead, d.behind, d.authors(ctx, in)), nil
}

// divergedAt reads dir's ahead/behind counts against its upstream; nil when there is no upstream or no divergence.
func divergedAt(ctx context.Context, dir string) (*divergence, error) {
	counts, ok, err := gitOut(ctx, dir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if err != nil || !ok {
		return nil, err // !ok: not a repo, detached, or no upstream
	}
	a, b := bothSides(counts[0])
	if a == 0 || b == 0 {
		return nil, nil
	}
	ids, _, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel", "HEAD", "@{upstream}", "--abbrev-ref", "HEAD",
		"@{upstream}")
	if len(ids) != 5 {
		return nil, err
	}
	return &divergence{top: ids[0], head: ids[1], up: ids[2], branch: ids[3], upName: ids[4], ahead: a, behind: b}, nil
}

// bothSides parses rev-list --left-right --count output; zeros for anything else.
func bothSides(line string) (ahead, behind int) {
	f := strings.Fields(line)
	if len(f) != 2 {
		return 0, 0
	}
	ahead, _ = strconv.Atoi(f[0])
	behind, _ = strconv.Atoi(f[1])
	return ahead, behind
}

// checkoutShared is true for the live config tree or a checkout a peer session works in.
func checkoutShared(ctx context.Context, dir string) (bool, error) {
	top, _, nested := checkoutOf(ctx, dir)
	if top == "" {
		return false, nil
	}
	if top == canonPath(paths.ConfigDir()) {
		return true, nil
	}
	peers, err := peersOf(ctx, top, nested)
	return len(peers) > 0, err
}

// authors names, from the claims ledger, whose commits sit on each side; "" when the ledger knows none.
func (d *divergence) authors(ctx context.Context, in *Input) string {
	byCommit := map[string]string{}
	self := paths.Slug(in.SessionID, "", 80, "session")
	_ = notices.ScanClaims(claimAlive, func(pid int, slug string, c notices.Claim) {
		if c.Commit != "" {
			byCommit[c.Commit] = notices.Label(pid, slug, c.Agent)
			if slug == self {
				byCommit[c.Commit] = "this session"
			}
		}
	})
	if len(byCommit) == 0 {
		return ""
	}
	var parts []string
	for _, side := range [][2]string{{"ahead", d.up + ".." + d.head}, {"behind", d.head + ".." + d.up}} {
		if who := committers(ctx, d.top, side[1], byCommit); len(who) > 0 {
			parts = append(parts, side[0]+": "+strings.Join(who, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// committers maps the commits in rng (at most 50) to the claimants whose short shas prefix them.
func committers(ctx context.Context, top, rng string, byCommit map[string]string) []string {
	shas, ok, err := gitOut(ctx, top, "rev-list", "--max-count=50", rng)
	if err != nil || !ok {
		return nil
	}
	var who []string
	for _, sha := range shas {
		for short, label := range byCommit {
			if len(short) >= 7 && strings.HasPrefix(sha, short) && !slices.Contains(who, label) {
				who = append(who, label)
			}
		}
	}
	slices.Sort(who)
	return who
}
