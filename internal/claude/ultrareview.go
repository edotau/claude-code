package claude

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Ultrareview is the scope of one `claude ultrareview`; Post is never sent, so findings stay local.
type Ultrareview struct {
	Base, Commit, Focus string
	Uncommitted, PR     bool
	JSON                bool
}

// Argv builds the ultrareview argument list; a scope the cloud review cannot express is refused.
func (u Ultrareview) Argv() ([]string, error) {
	args := []string{"ultrareview"}
	switch {
	case u.Uncommitted || u.Commit != "":
		return nil, errors.New("ultrareview reviews the branch, a base branch or a PR: --uncommitted and --commit are not supported")
	case u.Base != "" && u.PR:
		return nil, errors.New("at most one of --base, --pr")
	case strings.HasPrefix(u.Base, "-"):
		return nil, fmt.Errorf("--base ref %q must not start with -", u.Base)
	case u.PR:
		if _, err := strconv.ParseUint(u.Focus, 10, 32); err != nil {
			return nil, errors.New("ultrareview --pr takes the PR number: --pr 123")
		}
		args = append(args, u.Focus)
	case u.Focus != "":
		return nil, errors.New("ultrareview takes no focus words")
	case u.Base != "":
		args = append(args, u.Base)
	}
	if u.JSON {
		args = append(args, "--json")
	}
	return args, nil
}

// RunUltrareview runs `claude ultrareview` on Claude's own Anthropic login and returns its exit code.
func RunUltrareview(ctx context.Context, argv []string) int {
	bin, err := ClaudeBinary()
	if err != nil {
		return helptext.Fail("claude review: %v", err)
	}
	unset := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} // proxy and profile wiring: the cloud runs on the own login
	return agent.RunOwnLogin(ctx, "claude", bin, unset, argv)
}
