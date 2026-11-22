package agent

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
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

func (e *UnacceptedError) Error() string { return ErrModelRoute.Error() + ": " + e.Reason() }

// Reason is the refusal without the ErrModelRoute prefix, for callers that word their own.
func (e *UnacceptedError) Reason() string {
	var b strings.Builder
	switch e.Kind {
	case UnacceptedPrefixed:
		b.WriteString(e.Detail) // providers.PrefixError's text names the overlay
	case UnacceptedCold:
		fmt.Fprintf(&b, "%s: %s has no fresh listing and %s is not a catalog id; run `claude-code models preflight --live --profile %s`",
			e.Model, e.Profile, e.Model, e.Profile)
	default:
		fmt.Fprintf(&b, "%s: %s does not accept it for %s", e.Model, e.Profile, e.Agent)
	}
	if e.Detail != "" && e.Kind != UnacceptedPrefixed {
		textutil.WriteStrings(&b, " (", e.Detail, ")")
	}
	if len(e.Nearest) > 0 {
		textutil.WriteStrings(&b, "; nearest: ", strings.Join(e.Nearest, ", "))
	}
	return b.String()
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
	reg, _ := providers.Load()
	if e := prefixedRefusal(reg, agentName, p.Name, model); e != nil {
		return "", e
	}
	if model == "" || strings.EqualFold(model, AutoModel) {
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

// PrefixedRefusal is the Prefixed refusal for a `<x>:<model>` string on reg, else nil.
func PrefixedRefusal(reg *providers.Registry, agentName, model string) error {
	if e := prefixedRefusal(reg, agentName, "", model); e != nil {
		return e
	}
	return nil
}

// prefixedRefusal is providers.CheckModel's refusal as an UnacceptedError, which names provider and profile.
func prefixedRefusal(reg *providers.Registry, agentName, profile, model string) *UnacceptedError {
	model = strings.TrimSpace(model)
	if err := providers.CheckModel(reg, model); err != nil {
		return &UnacceptedError{Kind: UnacceptedPrefixed, Agent: agentName, Profile: profile, Model: model, Detail: err.Error()}
	}
	return nil
}

// judgeModel runs alias spelling and the listing, agent-menu and profile checks on a typed id.
func judgeModel(agentName string, p *providers.Provider, model string, now time.Time,
	refuse func(UnacceptedKind, string) error) (string, error) {
	target, alias, err := spellAccepted(agentName, p, model)
	if err != nil {
		return "", refuse(UnacceptedUnknown, err.Error())
	}
	wire, failed, skipped := checkModel(agentName, p, target, now)
	if failed != nil {
		return "", refuse(UnacceptedUnserved, failed.Detail)
	}
	if task, bad := nonChatEndpoint(p, stripEffort(agentName, wire), now); bad {
		return "", refuse(UnacceptedUnserved, wire+" is not a chat endpoint (task "+task+")")
	}
	return acceptVerdict(stripEffort(agentName, target), p, alias, skipped, refuse)
}

// acceptVerdict accepts a checked id: known to the profile, else unverified (skipped listing, or none to check).
func acceptVerdict(target string, p *providers.Provider, alias bool, skipped string,
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
	return "unverified: " + p.Name + " has no listing to check " + target + " against", nil // owner: upstream judges it
}

// stripEffort is id without the agent's native effort suffix (agy's <id>-<effort>), else id.
func stripEffort(agentName, id string) string {
	if base, _, ok := agentid.SplitNativeEffort(id); ok && agentid.Routing(agentName).EffortSuffix {
		return base
	}
	return id
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
	picked, _ := catalog.PickerModels(p)
	return slices.ContainsFunc(picked, func(m providers.ModelInfo) bool { return m.ID == id })
}

// AcceptedIDs is p's fresh READY chat ids the agent can run, else p's models then the agent's catalog ids (fresh false).
func AcceptedIDs(agentName string, p *providers.Provider, now time.Time) (ids []string, fresh bool) {
	if s, ok := serving.Fresh(p.Name, now); ok {
		chat := chatServed(s)
		for _, m := range s.Models {
			if _, why := preflightModelWire(agentName, p, m.ID); m.IsReady() && chat(m) && why == "" {
				ids = append(ids, m.ID)
			}
		}
		return ids, true
	}
	if gh, ok := githubSnapshot(p, now); ok {
		return slices.DeleteFunc(slices.Clone(gh), func(id string) bool { return id == AutoModel }), true
	}
	ids = slices.Compact(slices.Sorted(maps.Values(p.Models)))
	for _, id := range catalogPool(agentName) {
		if wire, why := preflightModelWire(agentName, p, id); why == "" && !slices.Contains(ids, wire) {
			ids = append(ids, wire)
		}
	}
	return ids, false
}

// catalogPool is the catalog ids agentName runs: those listing its efforts, else every id of its model family.
func catalogPool(agentName string) []string {
	if ids := models.AgentModels(agentName); len(ids) > 0 {
		return ids
	}
	fam := agentid.Routing(agentName).ModelFamily
	return slices.DeleteFunc(models.ModelIDs(), func(id string) bool {
		return fam != "" && string(models.FamilyKind(id)) != fam
	})
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
