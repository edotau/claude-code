package cli

import (
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memstore"
)

func memorySpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"memory": {sub: map[string]*compNode{
			"recall": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"),
				flag("load"), formatFlag()}},
			"remember": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), valuedFlag("type", memoryRememberTypes)}},
			"init":     {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"review": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("stale-days"), boolFlag("notice"),
				boolFlag("brief")}},
			"promote": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames),
				valuedFlag("to", memoryPromoteTargets), boolFlag("dry-run")}},
			"update":   {flags: []flagInfo{flag("transcript"), flag("cwd"), flag("session")}},
			"dream":    {flags: []flagInfo{flag("cwd"), boolFlag("force")}},
			"search":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"), formatFlag()}},
			"show":     {},
			"digest":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"index":    {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"path":     {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"fmt":      {flags: []flagInfo{boolFlag("check")}},
			"catchup":  {},
			"watch":    {flags: []flagInfo{flag("session"), flag("transcript"), flag("cwd"), flag("owner"), flag("idle")}},
			"timeline": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("n")}},
			"archive": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("dupes"), flag("split"),
				boolFlag("apply"), boolFlag("restore")}},
			"alias": {flags: []flagInfo{flag("repo"), valuedFlag("into", memoryRepoNames), boolFlag("dry-run")}},
		}},
	}
}

// memoryRememberTypes mirrors bank.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return []string{"user", "feedback", "project", "reference"} }

// memoryPromoteTargets mirrors memory.PromoteTargets for `memory promote --to` completion.
func memoryPromoteTargets() []string { return memory.PromoteTargets }

func memoryRepoNames() []string { return memstore.RepoKeys() }
