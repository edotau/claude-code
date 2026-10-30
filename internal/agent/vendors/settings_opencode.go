package vendors

import (
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// opencodeModule is opencode's own sub-pick knobs: the paid-backend ladder, the work-repo markers that
// block zen-free, which tiers zen-free may serve, and the free leg's stall deadline.
type opencodeModule struct{}

func (opencodeModule) Vendor() string { return "opencode" }

func (opencodeModule) Fields() []Field {
	return []Field{ladderField(), workRepoMarkersField(), freeTiersField(), freeFirstOutputField()}
}

// Env contributes nothing: opencode reads routing.opencode straight from the registry (internal/route,
// internal/agent), not through the environment.
func (opencodeModule) Env(providers.Routing) map[string]string { return nil }

func splitList(input string) []string {
	if strings.TrimSpace(input) == "" {
		return []string{}
	}
	var out []string
	for part := range strings.SplitSeq(input, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ladderField() Field {
	return Field{
		Key:   "opencode.ladder",
		Label: "ladder",
		Help:  "comma-separated paid backends, in try order (zen-free always runs first regardless)",
		Kind:  KindList,
		Get:   func(r providers.Routing) string { return strings.Join(r.Opencode.LadderOrder(), ",") },
		IsSet: func(r providers.Routing) bool { return r.Opencode.Ladder != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Opencode.Ladder = nil
				return nil
			}
			l := splitList(input)
			r.Opencode.Ladder = &l
			return nil
		},
	}
}

func workRepoMarkersField() Field {
	return Field{
		Key:   "opencode.workRepoMarkers",
		Label: "work-repo markers",
		Help:  "comma-separated remote-URL substrings marking a repo as work (zen-free never serves one)",
		Kind:  KindList,
		Get: func(r providers.Routing) string {
			m := r.Opencode.Markers()
			if len(m) == 0 {
				return "none (free models allowed in every repo)"
			}
			return strings.Join(m, ",")
		},
		IsSet: func(r providers.Routing) bool { return r.Opencode.WorkRepoMarkers != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Opencode.WorkRepoMarkers = nil
				return nil
			}
			l := splitList(input)
			r.Opencode.WorkRepoMarkers = &l
			return nil
		},
	}
}

var freeTierValues = []string{"sonnet,haiku", "haiku", "none"}

func freeTiersField() Field {
	return Field{
		Key:    "opencode.freeTiers",
		Label:  "free tiers",
		Help:   "which tiers zen-free may serve; opus is never eligible",
		Kind:   KindEnum,
		Values: freeTierValues,
		Get: func(r providers.Routing) string {
			sonnet, haiku := r.Opencode.FreeTierOK("sonnet"), r.Opencode.FreeTierOK("haiku")
			switch {
			case sonnet && haiku:
				return "sonnet,haiku"
			case haiku:
				return "haiku"
			default:
				return "none"
			}
		},
		IsSet: func(r providers.Routing) bool { return r.Opencode.FreeTiers != nil },
		Set: func(r *providers.Routing, input string) error {
			switch input {
			case "":
				r.Opencode.FreeTiers = nil
			case "sonnet,haiku":
				l := []string{"sonnet", "haiku"}
				r.Opencode.FreeTiers = &l
			case "haiku":
				l := []string{"haiku"}
				r.Opencode.FreeTiers = &l
			case "none":
				l := []string{}
				r.Opencode.FreeTiers = &l
			default:
				return fmt.Errorf("opencode.freeTiers: %q, want one of %s", input, strings.Join(freeTierValues, " | "))
			}
			return nil
		},
	}
}

func freeFirstOutputField() Field {
	return Field{
		Key:   "opencode.freeFirstOutput",
		Label: "free first-output cutoff",
		Help:  "zen-free must emit its first byte within this long, else fail over (a Go duration, e.g. 90s)",
		Kind:  KindText,
		Get:   func(r providers.Routing) string { return r.Opencode.FirstOutput().String() },
		IsSet: func(r providers.Routing) bool { return r.Opencode.FreeFirstOutput != "" },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Opencode.FreeFirstOutput = ""
				return nil
			}
			d, err := time.ParseDuration(input)
			if err != nil || d <= 0 {
				return fmt.Errorf("opencode.freeFirstOutput: %q must be a positive Go duration", input)
			}
			r.Opencode.FreeFirstOutput = input
			return nil
		},
	}
}
