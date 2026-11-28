// overlay_run.go — the overlay keys that shape one run's agent options (the folded ask and launcher flags).
package agentconfig

import (
	"cmp"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/routing/googletools"
)

// runKeys are per-run keys; a child inherits only restrictions; the ask path reads them from task 3.1a on.
type runKeys struct {
	Agent              string          `json:"agent,omitempty"`
	Agents             json.RawMessage `json:"agents,omitempty"`
	Tier               string          `json:"tier,omitempty"`
	Tools              *bool           `json:"tools,omitempty"`
	NoTools            *bool           `json:"noTools,omitempty"`
	NoPersist          *bool           `json:"noPersist,omitempty"`
	DebugFile          string          `json:"debugFile,omitempty"`
	SystemPrompt       string          `json:"systemPrompt,omitempty"`
	AppendSystemPrompt string          `json:"appendSystemPrompt,omitempty"`
	OutputSchema       string          `json:"outputSchema,omitempty"`
	Native             *bool           `json:"native,omitempty"`
	Autonomous         *bool           `json:"autonomous,omitempty"`
	Fleet              *bool           `json:"fleet,omitempty"`
	EnableMemory       *bool           `json:"enableMemory,omitempty"`
	GoogleTools        string          `json:"googleTools,omitempty"`
	Timeout            string          `json:"timeout,omitempty"`
	File               string          `json:"file,omitempty"` // launch only: codex config.toml whose keys ride as -c
}

// tierNames mirrors agent.TierNames (agent imports this package).
var tierNames = []string{"fast", "standard", "deep"}

// durationPattern is the Go duration syntax the schema and checkTimeout both enforce (no sign, a unit on every part).
const durationPattern = `^(?:(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:ns|us|µs|μs|ms|s|m|h))+$`

var durationRE = regexp.MustCompile(durationPattern)

// googleToolsPattern is a comma list of the built tool names, optional spaces after commas.
func googleToolsPattern() string {
	n := "(?:" + strings.ReplaceAll(googletools.Join(googletools.All), ",", "|") + ")"
	return "^" + n + "(?:, *" + n + ")*$"
}

var googleToolsRE = regexp.MustCompile(googleToolsPattern())

// flags is the tri-state bool keys; the first two are restrictions (once true they stay true).
func (k *runKeys) flags() []**bool {
	return []**bool{&k.NoTools, &k.NoPersist, &k.Native, &k.Autonomous, &k.Fleet, &k.EnableMemory}
}

// restrictions is the part of k a child inherits: it may only tighten these.
func (k runKeys) restrictions() runKeys {
	r := runKeys{Timeout: k.Timeout}
	if k.NoTools != nil && *k.NoTools {
		r.NoTools = new(true)
	}
	if k.NoPersist != nil && *k.NoPersist {
		r.NoPersist = new(true)
	}
	if k.Tools != nil && !*k.Tools {
		r.Tools = new(false)
	}
	return r
}

// merge lays add over k in place; k is the caller's own copy. tighten keeps k's restrictions: add cannot loosen them.
func (k *runKeys) merge(add runKeys, tighten bool) {
	was := k.Timeout
	k.Agent, k.Tier = cmp.Or(add.Agent, k.Agent), cmp.Or(add.Tier, k.Tier)
	k.DebugFile, k.File = cmp.Or(add.DebugFile, k.DebugFile), cmp.Or(add.File, k.File)
	k.SystemPrompt, k.AppendSystemPrompt = cmp.Or(add.SystemPrompt, k.SystemPrompt), cmp.Or(add.AppendSystemPrompt, k.AppendSystemPrompt)
	k.OutputSchema, k.GoogleTools = cmp.Or(add.OutputSchema, k.OutputSchema), cmp.Or(add.GoogleTools, k.GoogleTools)
	k.Timeout = cmp.Or(add.Timeout, k.Timeout)
	if tighten {
		k.Timeout = shorter(was, add.Timeout)
	}
	if len(add.Agents) > 0 {
		k.Agents = add.Agents
	}
	if add.Tools != nil && (!tighten || k.Tools == nil || *k.Tools) {
		k.Tools = add.Tools
	}
	have := add.flags()
	for i, p := range k.flags() {
		if a := *have[i]; a != nil && (!tighten || i >= 2 || *p == nil || !**p) {
			*p = a
		}
	}
}

// shorter is the smaller of two duration strings; an empty one is unset, an unparsable add is left for validation.
func shorter(cur, add string) string {
	a, errA := time.ParseDuration(cur)
	b, errB := time.ParseDuration(add)
	if cur == "" || add == "" || errA != nil || errB != nil || b < a {
		return cmp.Or(add, cur)
	}
	return cur
}

func (k *runKeys) detach() {
	k.Agents, k.Tools = slices.Clone(k.Agents), clonePtr(k.Tools)
	for _, p := range k.flags() {
		*p = clonePtr(*p)
	}
}

func (k runKeys) isZero() bool { return reflect.ValueOf(k).IsZero() }

// mergeRun lays add's bool controls and run keys over o; a child or fallback hop only tightens (sticky true).
func (o *Overlay) mergeRun(add Overlay, hop Hop) {
	o.CodexNetwork, o.Cloud = o.CodexNetwork || add.CodexNetwork, o.Cloud || add.Cloud
	o.merge(add.runKeys, hop != HopAppend && hop != HopResume)
}

// checkRun validates the run keys; an error names the key, never its value.
func (o Overlay) checkRun() error {
	for _, k := range []struct{ name, v string }{{"agent", o.Agent}, {"debugFile", o.DebugFile}, {"file", o.File}} {
		if k.v != "" && (strings.TrimSpace(k.v) == "" || strings.ContainsFunc(k.v, unicode.IsControl)) {
			return errors.New(k.name + ": blank or holds a control character")
		}
	}
	return cmp.Or(o.checkTierTools(), o.checkTimeout(), o.checkGoogleTools(), o.checkAgents())
}

func (o Overlay) checkTierTools() error {
	switch {
	case o.Tier != "" && !slices.Contains(tierNames, o.Tier):
		return errors.New("tier: want " + strings.Join(tierNames, ", "))
	case o.Tools != nil && *o.Tools && o.NoTools != nil && *o.NoTools:
		return errors.New("tools: not with noTools")
	}
	return nil
}

func (o Overlay) checkTimeout() error {
	if o.Timeout == "" {
		return nil
	}
	if d, err := time.ParseDuration(o.Timeout); err != nil || d <= 0 || !durationRE.MatchString(o.Timeout) {
		return errors.New("timeout: want a positive Go duration such as 9m30s")
	}
	return nil
}

func (o Overlay) checkGoogleTools() error {
	if o.GoogleTools == "" {
		return nil
	}
	if _, err := googletools.Parse(strings.Split(o.GoogleTools, ",")); err != nil || !googleToolsRE.MatchString(o.GoogleTools) {
		return errors.New("googleTools: want a comma list of " + googletools.Join(googletools.All))
	}
	return nil
}

func (o Overlay) checkAgents() error {
	if len(o.Agents) == 0 {
		return nil
	}
	var defs map[string]json.RawMessage
	if json.Unmarshal(o.Agents, &defs) != nil || defs == nil {
		return errors.New("agents: want an object of agent definitions")
	}
	return nil
}

// checkEmptyRun refuses an empty tier, timeout or googleTools in a document (a typed empty string reads as unset).
func checkEmptyRun(raw []byte) error {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	for name, v := range doc { // encoding/json matches field names case-insensitively
		for _, k := range []string{"tier", "timeout", "googleTools"} {
			if strings.EqualFold(name, k) && string(v) == `""` {
				return errors.New(k + ": empty")
			}
		}
	}
	return nil
}
