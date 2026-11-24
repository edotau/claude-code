package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

const (
	maxAutoLanes = 3
	staleBehind  = 20
)

var (
	benchLaneName   = regexp.MustCompile(`^bench-[a-z0-9._-]+-[0-9]{8}-[0-9]+$`)
	autofixLaneName = regexp.MustCompile(`^autofix-[0-9a-f]{7}$`)
	laneList        = func(ctx context.Context, top string) (string, error) {
		bin, err := laneBin()
		if err != nil {
			return "", err
		}
		out, _, err := driverRun(ctx, top, "", bin, "worktree", "list", "--format", "json")
		return out, err
	}
	laneClean = func(ctx context.Context, top, name string) (string, error) {
		bin, err := laneBin()
		if err != nil {
			return "", err
		}
		out, _, err := driverRun(ctx, top, "", bin, "worktree", "clean", name)
		return out, err
	}
)

// laneBin is this binary, refused under go test: a test binary would re-run itself as the lane verb without end.
func laneBin() (string, error) {
	bin, err := paths.SelfBinary()
	if err == nil && strings.HasSuffix(bin, ".test") {
		return "", errors.New("lane verbs need the claude-code binary, not a test binary")
	}
	return bin, err
}

// Lane is one `worktree list --format json` row this tool created (bench loop or autofix).
type Lane struct {
	Path   string
	Branch string
	Dirty  bool
	Merged bool
	Behind int
	Name   string `json:"-"`
}

// AutoLanes is the bench and autofix lanes only, matched by name and branch so a user's lane never shows.
func AutoLanes(ctx context.Context, top string) ([]Lane, error) {
	out, err := laneList(ctx, top)
	if err != nil {
		return nil, fmt.Errorf("worktree list: %w", err)
	}
	var listed []Lane
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return nil, fmt.Errorf("worktree list JSON: %w", err)
	}
	if listed == nil {
		return nil, errors.New("worktree list JSON: expected an array")
	}
	lanes := make([]Lane, 0, len(listed))
	for _, lane := range listed {
		name := filepath.Base(lane.Path)
		if (benchLaneName.MatchString(name) || autofixLaneName.MatchString(name)) && lane.Branch == "worktree-"+name {
			lane.Name = name
			lanes = append(lanes, lane)
		}
	}
	return lanes, nil
}

// LaneCap refuses a new automatic lane at the plan's cap: "At most 3 automatic lanes stay open; a fourth waits".
func LaneCap(ctx context.Context, top string) error {
	lanes, err := AutoLanes(ctx, top)
	if err != nil {
		return err
	}
	if len(lanes) < maxAutoLanes {
		return nil
	}
	names := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		names = append(names, lane.Name)
	}
	return fmt.Errorf("automatic lane cap: %d open (limit %d): %s", len(lanes), maxAutoLanes, strings.Join(names, ", "))
}

// CleanLanes removes merged clean lanes without force; a dirty or unmerged one is only reported, never discarded.
func CleanLanes(ctx context.Context, top string, out io.Writer) error {
	lanes, err := AutoLanes(ctx, top)
	if err != nil {
		return err
	}
	var failures []error
	for _, lane := range lanes {
		switch {
		case lane.Dirty:
			_, err = fmt.Fprintf(out, "kept dirty automatic lane %s\n", lane.Name)
		case !lane.Merged && lane.Behind > staleBehind:
			_, err = fmt.Fprintf(out, "kept stale automatic lane %s (%d behind, unmerged): claude-code worktree land %s\n",
				lane.Name, lane.Behind, lane.Name)
		case !lane.Merged:
			continue
		default:
			cleanOut, cleanErr := laneClean(ctx, top, lane.Name)
			if cleanErr != nil {
				failures = append(failures, fmt.Errorf("worktree clean %s: %w", lane.Name, cleanErr))
				reason := firstLine(cleanOut)
				if reason == "" {
					reason = firstLine(cleanErr.Error())
				}
				_, err = fmt.Fprintf(out, "refused automatic lane %s: %s\n", lane.Name, reason)
			} else {
				_, err = fmt.Fprintf(out, "removed automatic lane %s\n", lane.Name)
			}
		}
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	return errors.Join(failures...)
}
