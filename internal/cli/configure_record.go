package cli

import (
	"fmt"
	"os"
	"strings"

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
	set, err := parseRecordPairs(pairs)
	if err != nil {
		return helptext.Fail("configure agents: %v", err)
	}
	return applyRecord(agent, set, dryRun)
}

// parseRecordPairs reads key=value words into record fields; an empty value clears the field.
func parseRecordPairs(words []string) (map[string]string, error) {
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
		}
		if !ok || field == "" {
			return nil, fmt.Errorf("%q is not profile=<p>, model=<m>, fallback=<p> or config_route=<r>", w)
		}
		set[field] = strings.TrimSpace(val)
	}
	return set, nil
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
	for _, k := range recordKeys {
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
