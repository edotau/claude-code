package databricks

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Spec is the completion node for the databricks verb.
func Spec() *helptext.Node {
	flag, boolFlag, valuedFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag
	profileNames := providers.ProfileNames
	identities := func() []string { return []string{"user", "sp"} }
	return &helptext.Node{Flags: []helptext.Flag{valuedFlag("profile", profileNames)}, Sub: map[string]*helptext.Node{
		"check": {Flags: []helptext.Flag{boolFlag("all"), valuedFlag("profile", profileNames), boolFlag("1m"), flag("model"),
			valuedFlag("wire", func() []string { return []string{"serving", "ai-gateway", "all"} })}},
		"login": {Args: []func() []string{profileNames}, Flags: []helptext.Flag{flag("auth-type"), boolFlag("dry-run"), boolFlag("force"), valuedFlag("as", identities)}},
		"mint":  {Flags: []helptext.Flag{valuedFlag("profile", profileNames), boolFlag("dry-run"), boolFlag("copy"), valuedFlag("as", identities)}},
		"import": {Flags: []helptext.Flag{flag("dbxcfg"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
			boolFlag("seal-secrets"), boolFlag("apply")}},
		"export": {Flags: []helptext.Flag{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
		"config": {Args: []func() []string{func() []string { return []string{"claude", "codex"} }}, Flags: dbxCfgFlags(true)},
		"rescue": {Flags: append(dbxCfgFlags(false), boolFlag("print"))},
	}}
}

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []helptext.Flag {
	fs := []helptext.Flag{{Name: "profile", Values: providers.ProfileNames}, {Name: "host"}, {Name: "wire"},
		{Name: "model"}, {Name: "auth"}, {Name: "set"}, {Name: "unset"}}
	if withOut {
		fs = append(fs, helptext.Flag{Name: "out"}, helptext.Flag{Name: "force", IsBool: true})
	}
	return fs
}
