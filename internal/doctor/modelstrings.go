package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
)

func checkModelStrings(reg *providers.Registry, report func(bool, bool, string, string)) {
	c := modelStringsCheck(reg)
	report(c.OK, c.Hard, c.Name, c.Detail)
}

func modelStringsCheck(reg *providers.Registry) check {
	c := check{Name: "model strings", Detail: "no profile-prefixed stored models"}
	if reg == nil {
		c.Detail = "cannot scan without the provider registry"
		return c
	}
	values := map[string]string{}
	var problems []string
	if err := storedModelValues(values); err != nil {
		problems = append(problems, "vault: could not read stored model configuration")
	}
	if err := settingsModelValues(values); err != nil {
		problems = append(problems, settings.SettingsFile()+": could not read model env")
	}
	hits := prefixedModels(reg, values)
	c.OK = len(hits) == 0 && len(problems) == 0
	if len(hits) > 0 {
		problems = append(problems, fmt.Sprintf("%d prefixed model(s): %s", len(hits), strings.Join(hits, "; ")))
	}
	if len(problems) > 0 {
		c.Detail = strings.Join(problems, "; ")
	}
	return c
}

func storedModelValues(values map[string]string) error {
	return vault.View(func(tx *vault.Tx) error {
		for _, bucket := range []string{vault.Env, vault.Pins} {
			addModelEnv(values, "vault:"+bucket, tx.All(bucket))
		}
		raw, found := tx.Get(vault.Overlay, vault.AgentRecordsKey)
		if !found || raw == "" {
			return nil
		}
		var records map[string]vault.AgentRecord
		if err := json.Unmarshal([]byte(raw), &records); err != nil {
			return err
		}
		for name, record := range records {
			place := "vault:overlay/" + vault.AgentRecordsKey + "." + name
			values[place+".model"] = record.Model
			for alias, value := range record.Aliases {
				values[place+".aliases."+alias] = value
			}
		}
		return nil
	})
}

func settingsModelValues(values map[string]string) error {
	path := settings.SettingsFile()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	addModelEnv(values, path+": env", doc.Env)
	return nil
}

func addModelEnv(values map[string]string, place string, env map[string]string) {
	for key, value := range env {
		if modelEnvKey(key) {
			values[place+"."+key] = value
		}
	}
}

func modelEnvKey(key string) bool {
	if key == "CLAUDE_CODE_AUTO_MODE_MODEL" || key == "ANTHROPIC_MODEL" {
		return true
	}
	if strings.HasPrefix(key, "ANTHROPIC_") && strings.HasSuffix(key, "_MODEL") {
		return true
	}
	for _, name := range agentid.All() {
		if key == strings.ToUpper(name)+"_MODEL" {
			return true
		}
	}
	return false
}

func prefixedModels(reg *providers.Registry, values map[string]string) []string {
	var hits []string
	for _, place := range slices.Sorted(maps.Keys(values)) {
		if e, ok := errors.AsType[*providers.PrefixError](providers.CheckModel(reg, values[place])); ok { // the overlay only: a stored value stays private
			hits = append(hits, place+": use --overlay '"+e.Overlay()+"' --model <model>")
		}
	}
	return hits
}
