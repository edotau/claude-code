package databricks

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/providers"
)

// Spec is the completion node for the databricks verb.
func Spec() *completion.Node {
	flag, boolFlag, valuedFlag := completion.Plain, completion.BoolFlag, completion.ValuedFlag
	profileNames := providers.ProfileNames
	identities := func() []string { return []string{"user", "sp"} }
	return &completion.Node{Flags: []completion.Flag{valuedFlag("profile", profileNames)}, Sub: map[string]*completion.Node{
		"check": {Flags: []completion.Flag{boolFlag("all"), valuedFlag("profile", profileNames), boolFlag("1m"), flag("model")}},
		"login": {Args: []func() []string{profileNames}, Flags: []completion.Flag{flag("auth-type"), boolFlag("dry-run"), boolFlag("force"), valuedFlag("as", identities)}},
		"mint":  {Flags: []completion.Flag{valuedFlag("profile", profileNames), boolFlag("dry-run"), boolFlag("copy"), valuedFlag("as", identities)}},
		"import": {Flags: []completion.Flag{flag("dbxcfg"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
			boolFlag("seal-secrets"), boolFlag("apply")}},
		"export": {Flags: []completion.Flag{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
		"config": {Args: []func() []string{func() []string { return []string{"claude", "codex"} }}, Flags: dbxCfgFlags(true)},
		"rescue": {Flags: append(dbxCfgFlags(false), boolFlag("print"))},
	}}
}

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []completion.Flag {
	fs := []completion.Flag{{Name: "profile", Values: providers.ProfileNames}, {Name: "host"}, {Name: "wire"},
		{Name: "model"}, {Name: "auth"}, {Name: "set"}, {Name: "unset"}}
	if withOut {
		fs = append(fs, completion.Flag{Name: "out"}, completion.Flag{Name: "force", IsBool: true})
	}
	return fs
}
