package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// recordKeys maps each `agents record` key to the agent record field it writes, in the order they are applied.
var recordKeys = []struct{ key, field string }{{"profile", "PROFILE"}, {"model", "MODEL"}, {"fallback", "FALLBACK"}}

// cmdAgentsRecord is `agents record <agent> key=value...`: set an agent record's profile, model or fallback.
func cmdAgentsRecord(args []string) int {
	fs := agentsFlags("record")
	dryRun := fs.Bool("dry-run", false, "print the change; write nothing")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) < 2 {
		fs.Usage()
		return 2
	}
	set, err := parseRecordPairs(words[1:])
	if err != nil {
		return helptext.Fail("agents record: %v", err)
	}
	return applyRecord(words[0], set, *dryRun)
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
		if !ok || field == "" {
			return nil, fmt.Errorf("%q is not profile=<p>, model=<m> or fallback=<p>", w)
		}
		set[field] = strings.TrimSpace(val)
	}
	return set, nil
}

// applyRecord validates every field first, then prints (and unless dryRun writes) each change as "agent key: old → new".
func applyRecord(agent string, set map[string]string, dryRun bool) int {
	recs, err := vault.ReadAgentRecords([]string{agent})
	if err != nil {
		return helptext.Fail("agents record: %v", err)
	}
	for field, val := range set {
		if err := configure.ValidateAgentField(agent, field, val); err != nil {
			return helptext.Fail("agents record: %v", err)
		}
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
			return helptext.Fail("agents record: %s: %v", k.key, err)
		}
		fmt.Fprintln(os.Stderr, line)
	}
	return 0
}
