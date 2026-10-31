package agent

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

//go:embed template/opencode.json template/opencode-tui.json
var opencodeOverlayFS embed.FS

var _ = schemas.Register(schemas.Format{
	Name: "opencode-overlay", Owner: "internal/agent", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(opencodeOverlayDir(), "*.json") },
	Version: 1, Keep: "re-rendered from the template on every launch; opencode reads it, so no schema field",
})

// opencodeOverlayDir is state/harness/opencode: the one place the harness writes for opencode, never
// opencode's own ~/.config/opencode (configure/theme apply write there only for the themes file).
func opencodeOverlayDir() string { return filepath.Join(paths.StateDir(), "opencode") }

// OpencodeOverlayRendered computes the overlay's two files without writing them: cfgBody is the tracked
// template with model/small_model overwritten from the routing table's opencode sonnet/haiku cells (the
// template's own values are only a fallback for a table load error); tuiBody keeps a theme `theme apply`
// already wrote at tuiPath, else the template's.
func OpencodeOverlayRendered() (cfgPath string, cfgBody []byte, tuiPath string, tuiBody []byte, err error) {
	dir := opencodeOverlayDir()
	cfgBody, err = opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		return "", nil, "", nil, err
	}
	cfgBody = applyOpencodeTableModels(cfgBody)
	tuiTemplate, err := opencodeOverlayFS.ReadFile("template/opencode-tui.json")
	if err != nil {
		return "", nil, "", nil, err
	}
	cfgPath = filepath.Join(dir, "opencode.json")
	tuiPath = filepath.Join(dir, "tui.json")
	tuiBody = preserveAppliedTheme(tuiPath, tuiTemplate)
	return cfgPath, cfgBody, tuiPath, tuiBody, nil
}

// applyOpencodeTableModels overwrites model/small_model from the routing table's opencode sonnet/haiku
// cells; a table load error leaves the template's own hard-coded values (its one fallback use) untouched.
func applyOpencodeTableModels(cfgBody []byte) []byte {
	mainModel, smallModel, ok := opencodeOverlayModels()
	if !ok {
		return cfgBody
	}
	var doc map[string]any
	if json.Unmarshal(cfgBody, &doc) != nil {
		return cfgBody
	}
	doc["model"], doc["small_model"] = mainModel, smallModel
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(doc) != nil {
		return cfgBody
	}
	return buf.Bytes()
}

// WriteOpencodeOverlay writes the tracked opencode.json/tui.json templates to state/harness/opencode
// and returns their paths; a theme already applied into the existing tui.json (theme apply) survives.
func WriteOpencodeOverlay() (cfg, tui string, err error) {
	cfgPath, cfgBody, tuiPath, tuiBody, err := OpencodeOverlayRendered()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return "", "", err
	}
	if _, err := paths.WriteIfChanged(cfgPath, cfgBody, 0o600); err != nil {
		return "", "", err
	}
	if _, err := paths.WriteIfChanged(tuiPath, tuiBody, 0o600); err != nil {
		return "", "", err
	}
	return cfgPath, tuiPath, nil
}

// preserveAppliedTheme keeps a theme `theme apply` already wrote into the on-disk tui.json: it reads
// the existing file's "theme" key and overrides the template's before writing, so a repeated overlay
// write (every opencode launch) never clobbers a theme the owner picked after the template rendered.
func preserveAppliedTheme(path string, template []byte) []byte {
	cur, err := os.ReadFile(path)
	if err != nil {
		return template
	}
	var curDoc map[string]any
	if json.Unmarshal(cur, &curDoc) != nil {
		return template
	}
	theme, ok := curDoc["theme"].(string)
	if !ok {
		return template
	}
	var next map[string]any
	if json.Unmarshal(template, &next) != nil {
		return template
	}
	if next["theme"] == theme {
		return template // already matches; skip the marshal round-trip (it would HTML-escape the keybinds)
	}
	next["theme"] = theme
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(next) != nil {
		return template
	}
	return buf.Bytes()
}

// opencodeModule is opencode's own sub-pick knobs: the paid-backend ladder, the work-repo markers that
// block zen-free, which tiers zen-free may serve, and the free leg's stall deadline.
type opencodeModule struct{}

func (opencodeModule) Name() string { return "opencode" }

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
