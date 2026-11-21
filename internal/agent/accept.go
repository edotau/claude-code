package agent

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
)

// UnacceptedKind says why AcceptModel refused a model.
type UnacceptedKind string

const (
	UnacceptedPrefixed UnacceptedKind = "prefixed" // a <profile>:<model> string
	UnacceptedUnserved UnacceptedKind = "unserved" // a fresh listing or the agent's menu lacks it
	UnacceptedCold     UnacceptedKind = "cold"     // no fresh listing and the id is neither catalog nor databricks-
	UnacceptedUnknown  UnacceptedKind = "unknown"  // no spelling this profile or agent knows
)

// UnacceptedError is AcceptModel's refusal; it wraps ErrModelRoute.
type UnacceptedError struct {
	Kind                  UnacceptedKind
	Agent, Profile, Model string
	Detail                string
	Nearest               []string
}

func (e *UnacceptedError) Unwrap() error { return ErrModelRoute }

func (e *UnacceptedError) Error() string {
	var b strings.Builder
	switch e.Kind {
	case UnacceptedPrefixed:
		fmt.Fprintf(&b, "%s: a model is never <profile>:<model>; use --overlay '{\"provider\":\"…\",\"profile\":\"…\"}' --model <id>", e.Model)
	case UnacceptedCold:
		fmt.Fprintf(&b, "%s: %s has no fresh listing and %s is not a catalog id; run `claude-code models preflight --live --profile %s`",
			e.Model, e.Profile, e.Model, e.Profile)
	default:
		fmt.Fprintf(&b, "%s: %s does not accept it for %s", e.Model, e.Profile, e.Agent)
	}
	if e.Detail != "" && e.Kind != UnacceptedPrefixed {
		b.WriteString(" (" + e.Detail + ")")
	}
	if len(e.Nearest) > 0 {
		b.WriteString("; nearest: " + strings.Join(e.Nearest, ", "))
	}
	return ErrModelRoute.Error() + ": " + b.String()
}

// AcceptModel decides whether agentName may run model on p; a non-empty note means accepted without a listing check.
func AcceptModel(agentName string, p *providers.Provider, model string, now time.Time) (note string, err error) {
	model = strings.TrimSpace(model)
	refuse := func(k UnacceptedKind, detail string) error {
		e := &UnacceptedError{Kind: k, Agent: agentName, Profile: p.Name, Model: model, Detail: detail}
		if k != UnacceptedPrefixed {
			e.Nearest = nearestAccepted(agentName, p, model, now)
		}
		return e
	}
	if reg, rerr := providers.Load(); rerr == nil {
		if c, cerr := providers.CastModel(reg, model); cerr != nil || c.Profile != "" {
			return "", refuse(UnacceptedPrefixed, "")
		}
	}
	if _, auto := SplitAuto(model); model == "" || auto {
		return "", nil
	}
	word, werr := resolveModelWord(ModelQuery{Agent: agentName, Model: model})
	if werr != nil {
		return "", refuse(UnacceptedUnknown, werr.Error())
	}
	if word != model {
		return "", nil
	}
	return judgeModel(agentName, p, model, now, refuse)
}

// judgeModel runs alias spelling and the listing, agent-menu and profile checks on a typed id.
func judgeModel(agentName string, p *providers.Provider, model string, now time.Time,
	refuse func(UnacceptedKind, string) error) (string, error) {
	target, alias, err := spellAccepted(agentName, p, model)
	if err != nil {
		return "", refuse(UnacceptedUnknown, err.Error())
	}
	_, failed, skipped := checkModel(agentName, p, target, now)
	if failed != nil {
		return "", refuse(UnacceptedUnserved, failed.Detail)
	}
	return acceptVerdict(p, target, alias, skipped, refuse)
}

// acceptVerdict accepts a checked id: unverified when the listing was skipped, else known to the profile.
func acceptVerdict(p *providers.Provider, target string, alias bool, skipped string,
	refuse func(UnacceptedKind, string) error) (string, error) {
	_, _, catalogID := models.Resolve(target)
	known := alias || catalogID
	switch {
	case skipped != "" && !known && !strings.HasPrefix(target, models.DatabricksPrefix):
		return "", refuse(UnacceptedCold, "")
	case skipped != "":
		return "unverified: " + skipped, nil
	case known || p.Preset == providers.PresetDatabricks || ownsModel(p, target):
		return "", nil
	}
	return "", refuse(UnacceptedUnknown, "not in the profile's models or saved choices")
}

// spellAccepted is model's wire spelling on p when it is a family word or alias; alias says which.
func spellAccepted(agentName string, p *providers.Provider, model string) (target string, alias bool, err error) {
	name := strings.ToLower(models.Strip1M(model))
	switch {
	case providers.IsFamily(name):
		return providers.ConcreteModel(agentName, p, name), true, nil
	case slices.Contains(models.AliasNames, name):
		target, err = spellAlias(&ModelRoute{}, p, agentName, model, name)
		return target, true, err
	}
	return model, false, nil
}

// ownsModel is rule 7: id is one of p's model values or saved discovery choices.
func ownsModel(p *providers.Provider, id string) bool {
	for _, v := range p.Models {
		if v == id {
			return true
		}
	}
	picked, _ := providers.PickerModels(p)
	return slices.ContainsFunc(picked, func(m providers.ModelInfo) bool { return m.ID == id })
}

// AcceptedIDs is p's fresh READY ids the agent can run, else the agent's catalog ids spelled for p (fresh false).
func AcceptedIDs(agentName string, p *providers.Provider, now time.Time) (ids []string, fresh bool) {
	if s, ok := router.FreshServed(p.Name, now); ok {
		for _, m := range s.Models {
			if _, why := preflightModelWire(agentName, p, m.ID); m.IsReady() && why == "" {
				ids = append(ids, m.ID)
			}
		}
		return ids, true
	}
	for _, id := range models.AgentModels(agentName) {
		ids = append(ids, providers.SpellPin(p, id))
	}
	return ids, false
}

// nearestAccepted is up to maxAlternatives ids: model's served family first, then the closest accepted id.
func nearestAccepted(agentName string, p *providers.Provider, model string, now time.Time) []string {
	out := servedAlternatives(p, model, now)
	ids, _ := AcceptedIDs(agentName, p, now)
	if n, ok := textutil.Nearest(model, append(ids, models.AliasNames...)); ok && !slices.Contains(out, n) {
		out = append([]string{n}, out...)
	}
	if len(out) == 0 {
		out = ids
	}
	return out[:min(len(out), maxAlternatives)]
}
