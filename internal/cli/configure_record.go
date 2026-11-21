package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// recordKeys maps each `configure agents` key to the agent record field it writes, in the order they are applied.
var recordKeys = []struct{ key, field string }{{"profile", "PROFILE"}, {"model", "MODEL"}, {"fallback", "FALLBACK"},
	{"account", "ACCOUNT"}, {"route", "ROUTE"}, {"fallbackRoute", "FALLBACKROUTE"}}

// routeKey is codex's record-only config_route preference, set here beside the pin keys.
const routeKey = "config_route"

// configureRecord is `configure agents <agent> key=value...`: set an agent record's profile, model or fallback.
func configureRecord(agent string, pairs []string, dryRun bool) int {
	set, err := parseRecordPairs(agent, pairs)
	if err != nil {
		return helptext.Fail("configure agents: %v", err)
	}
	return applyRecord(agent, set, dryRun)
}

// parseRecordPairs reads key=value words into record fields; an empty value clears the field.
func parseRecordPairs(agent string, words []string) (map[string]string, error) {
	set := map[string]string{}
	for _, w := range words {
		key, val, ok := strings.Cut(w, "=")
		field := ""
		for _, k := range recordKeys {
			if k.key == key {
				field = k.field
			}
		}
		if key == routeKey {
			field = routeKey
		} else if _, declared := agentpkg.SchemaFor(agent).CLI.Extras[key]; declared && field == "" {
			field = configure.ExtraPrefix + key
		}
		if !ok || field == "" {
			return nil, fmt.Errorf("%q is not <key>=<value> with key one of %s", w, strings.Join(recordKeyNames(agent), ", "))
		}
		set[field] = strings.TrimSpace(val)
	}
	return set, nil
}

// recordKeyNames lists the keys `configure agents` takes: the pin keys, config_route and agent's declared extras.
func recordKeyNames(agent string) []string {
	names := []string{"profile", "model", "fallback", "account", "route", "fallbackRoute", routeKey}
	for k := range agentpkg.SchemaFor(agent).CLI.Extras {
		if k != routeKey {
			names = append(names, k)
		}
	}
	return slices.Sorted(slices.Values(names))
}

// appliedKeys is recordKeys then the extras keys in set, sorted.
func appliedKeys(set map[string]string) []struct{ key, field string } {
	out := slices.Clone(recordKeys)
	for _, f := range slices.Sorted(maps.Keys(set)) {
		if key, ok := strings.CutPrefix(f, configure.ExtraPrefix); ok {
			out = append(out, struct{ key, field string }{key, f})
		}
	}
	return out
}

// applyRecord validates every field first, then prints (unless dryRun, writes) each change as "agent key: old → new".
func applyRecord(agent string, set map[string]string, dryRun bool) int {
	recs, err := vault.ReadAgentRecords([]string{agent})
	if err != nil {
		return helptext.Fail("configure agents: %v", err)
	}
	route, hasRoute := set[routeKey]
	delete(set, routeKey)
	if err := castRecordModel(set); err != nil {
		return helptext.FailUsage("configure agents: %v", err)
	}
	if err := validateRecordSet(agent, set, route, hasRoute); err != nil {
		return helptext.Fail("configure agents: %v", err)
	}
	for _, k := range appliedKeys(set) {
		val, ok := set[k.field]
		if !ok {
			continue
		}
		old := configure.AgentField(recs[agent], k.field)
		if isRouteField(k.field) {
			old = routeText(recs[agent], k.field)
		}
		line := fmt.Sprintf("%s %s: %s → %s", agent, k.key, orUnset(old), orUnset(val))
		if dryRun {
			fmt.Printf("would set %s; nothing written\n", line)
			continue
		}
		if err := setField(agent, k.field, val); err != nil {
			return helptext.Fail("configure agents: %s: %v", k.key, err)
		}
		fmt.Fprintln(os.Stderr, line)
	}
	if hasRoute {
		return applyRoute(route, dryRun)
	}
	return 0
}

// castRecordModel stores model=<profile>:<id> as profile=<profile> and model=<id>; the pool is the pool pin.
func castRecordModel(set map[string]string) error {
	v, ok := set["MODEL"]
	c, err := providers.CastShared(v)
	if !ok || err != nil || c.Profile == "" {
		return err
	}
	profile := c.Profile
	if profile == "auto" {
		profile = agentpkg.PoolPin
	}
	if cur, typed := set["PROFILE"]; typed && cur != profile {
		return fmt.Errorf("model %s names profile %s, but profile=%s; drop one", v, profile, cur)
	}
	if err := routeAgrees(set, v, c); err != nil {
		return err
	}
	set["MODEL"], set["PROFILE"] = c.Model, profile
	return nil
}

// routeAgrees refuses a typed route= or account= (a clear included) naming another route than model's prefix.
func routeAgrees(set map[string]string, model string, c providers.ModelCast) error {
	want := route.Route{Provider: c.Provider, Profile: c.Profile}
	if val, typed := set["ROUTE"]; typed {
		if rt, err := parseRouteObject("ROUTE", val); val == "" || err == nil && (rt.Provider != want.Provider || rt.Profile != want.Profile) {
			return fmt.Errorf("model %s names route %s, but route=%s; drop one", model, route.Target{Route: want}.JSON(), orUnset(val))
		}
	}
	if val, typed := set["ACCOUNT"]; typed && c.Provider != "github" {
		return fmt.Errorf("model %s names profile %s, but account=%s; drop one", model, c.Profile, orUnset(val))
	}
	return nil
}

// validateRecordSet checks every pin field and the config_route (codex only, a profile that serves it) before any write.
func validateRecordSet(agent string, set map[string]string, route string, hasRoute bool) error {
	if hasRoute {
		if agent != agentid.Codex {
			return fmt.Errorf("%s is codex only", routeKey)
		}
		if err := codex.ValidateConfigRoute(route); err != nil {
			return err
		}
	}
	for field, val := range set {
		if err := validateField(agent, field, val); err != nil {
			return err
		}
		if field == "MODEL" {
			if err := configure.ValidateAgentModel(agent, recordProfile(agent, set), val); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordProfile is the profile a model is judged on: the set's, else the record's; "" falls to the agent's own row.
func recordProfile(agent string, set map[string]string) string {
	if p, ok := set["PROFILE"]; ok {
		if p == agentpkg.PoolPin {
			return ""
		}
		return p
	}
	recs, err := vault.ReadAgentRecords([]string{agent})
	if err != nil {
		return ""
	}
	return configure.AgentField(recs[agent], "PROFILE")
}

func isRouteField(field string) bool {
	switch field {
	case "PROFILE", "FALLBACK", "ACCOUNT", "ROUTE", "FALLBACKROUTE":
		return true
	}
	return false
}

func isFallbackField(field string) bool { return field == "FALLBACK" || field == "FALLBACKROUTE" }

func validateField(agent, field, val string) error {
	if field == "ACCOUNT" || field == "ROUTE" || field == "FALLBACKROUTE" {
		_, _, err := resolveRoute(field, val)
		return err
	}
	return configure.ValidateAgentField(agent, field, val)
}

// routeText is the record's effective route for field as one-line JSON, "" when unset.
func routeText(rec vault.AgentRecord, field string) string {
	rt := rec.EffectiveRoute()
	if isFallbackField(field) {
		rt = rec.EffectiveFallbackRoute()
	}
	if rt.IsZero() {
		return ""
	}
	return route.Target{Route: rt}.JSON()
}

// resolveRoute turns a route-field value into the route to store; legacy means a pin with no registry row to fill from.
func resolveRoute(field, val string) (rt route.Route, legacy bool, err error) {
	switch {
	case val == "":
		return route.Route{}, false, nil
	case field == "ACCOUNT":
		rt = route.Route{Provider: "github", Account: val}
	case field == "ROUTE" || field == "FALLBACKROUTE":
		if rt, err = parseRouteObject(field, val); err != nil {
			return rt, false, err
		}
	default:
		reg, err := providers.Load()
		if err != nil {
			return rt, false, err
		}
		if rt = configure.RouteIn(reg, val); rt.IsZero() {
			return rt, true, nil
		}
	}
	return rt, false, rt.Validate()
}

func parseRouteObject(field, val string) (rt route.Route, err error) {
	name := strings.ToLower(field)
	dec := json.NewDecoder(strings.NewReader(val))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rt); err != nil {
		return rt, fmt.Errorf("%s: want a JSON route object like {\"provider\":\"databricks\",\"profile\":\"qa\"}: %v", name, err)
	}
	if rt.Provider == "" || rt.Profile == "" && rt.Account == "" {
		return rt, fmt.Errorf("%s: a route names provider and profile", name)
	}
	return rt, nil
}

// setField writes one record field; route fields store the route object and clear the legacy keys they replace.
func setField(agent, field, val string) error {
	if !isRouteField(field) {
		_, err := configure.SetAgentPin(agent, field, val)
		return err
	}
	rt, legacy, err := resolveRoute(field, val)
	if err != nil {
		return err
	}
	return vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		r := m[agent]
		if !isFallbackField(field) {
			m[agent] = configure.WithProfile(r, rt, val)
			return
		}
		r.Fallback, r.FallbackRoute = "", rt
		if legacy {
			r.Fallback = val
		}
		m[agent] = r
	})
}

// applyRoute prints (unless dryRun, writes) the codex config_route change as "codex config_route: old → new".
func applyRoute(route string, dryRun bool) int {
	prefs, err := codex.LoadCodexPreferences()
	if err != nil {
		return helptext.Fail("configure agents: %v", err)
	}
	line := fmt.Sprintf("codex %s: %s → %s", routeKey, orUnset(prefs[routeKey]), orUnset(route))
	if dryRun {
		fmt.Printf("would set %s; nothing written\n", line)
		return 0
	}
	err = codex.UpdateCodexPreferences(func(p map[string]string) map[string]string {
		if route == "" {
			delete(p, routeKey)
		} else {
			p[routeKey] = route
		}
		return p
	})
	if err != nil {
		return helptext.Fail("configure agents: %s: %v", routeKey, err)
	}
	fmt.Fprintln(os.Stderr, line)
	return 0
}
