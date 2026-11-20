package cli

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// recordKeys maps each `configure agents` key to the agent record field it writes, in the order they are applied.
var recordKeys = []struct{ key, field string }{{"profile", "PROFILE"}, {"model", "MODEL"}, {"fallback", "FALLBACK"}}

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
	names := []string{"profile", "model", "fallback", routeKey}
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
	if err := validateRecordSet(agent, set, route, hasRoute); err != nil {
		return helptext.Fail("configure agents: %v", err)
	}
	for _, k := range appliedKeys(set) {
		val, ok := set[k.field]
		if !ok {
			continue
		}
		old := configure.AgentField(recs[agent], k.field)
		line := fmt.Sprintf("%s %s: %s → %s", agent, k.key, orUnset(old), orUnset(val))
		if dryRun {
			fmt.Printf("would set %s; nothing written\n", line)
			continue
		}
		if _, err := configure.SetAgentPin(agent, k.field, val); err != nil {
			return helptext.Fail("configure agents: %s: %v", k.key, err)
		}
		fmt.Fprintln(os.Stderr, line)
	}
	if hasRoute {
		return applyRoute(route, dryRun)
	}
	return 0
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
		if err := configure.ValidateAgentField(agent, field, val); err != nil {
			return err
		}
	}
	return nil
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
