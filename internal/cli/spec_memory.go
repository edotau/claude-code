package cli

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memstore"
)

func memorySpecNodes() *completion.Node {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return &completion.Node{Sub: map[string]*completion.Node{
		"recall": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"),
			flag("load"), formatFlag()}},
		"remember": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), valuedFlag("type", memoryRememberTypes)}},
		"init":     {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"review": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), flag("stale-days"), boolFlag("notice"),
			boolFlag("brief")}},
		"promote": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames),
			valuedFlag("to", memoryPromoteTargets), boolFlag("dry-run")}},
		"update":   {Flags: []completion.Flag{flag("transcript"), flag("cwd"), flag("session")}},
		"dream":    {Flags: []completion.Flag{flag("cwd"), boolFlag("force")}},
		"search":   {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"), formatFlag()}},
		"show":     {},
		"digest":   {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"index":    {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"path":     {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"fmt":      {Flags: []completion.Flag{boolFlag("check")}},
		"catchup":  {},
		"watch":    {Flags: []completion.Flag{flag("session"), flag("transcript"), flag("cwd"), flag("owner"), flag("idle")}},
		"timeline": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), flag("n")}},
		"archive": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), boolFlag("dupes"), flag("split"),
			boolFlag("apply"), boolFlag("restore")}},
		"alias": {Flags: []completion.Flag{flag("repo"), valuedFlag("into", memoryRepoNames), boolFlag("dry-run")}},
	}}
}

// memoryRememberTypes mirrors bank.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return []string{"user", "feedback", "project", "reference"} }

// memoryPromoteTargets mirrors memory.PromoteTargets for `memory promote --to` completion.
func memoryPromoteTargets() []string { return memory.PromoteTargets }

func memoryRepoNames() []string { return memstore.RepoKeys() }
