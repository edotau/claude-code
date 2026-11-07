package cli

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

func vaultSections() []string {
	return []string{"secrets", "pins", "env", "providers", "agents", "settings", "vault", "all"}
}

func integrationSpecNodes() map[string]*completion.Node {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*completion.Node{
		"models": {Flags: []completion.Flag{valuedFlag("profile", providerNames), boolFlag("unpin"), boolFlag("live"),
			valuedFlag("pin", modelPinAliases), boolFlag("dry-run"), formatFlag()}},
		"vault": {Sub: map[string]*completion.Node{
			"list": {Args: []func() []string{vaultSections}, Flags: []completion.Flag{boolFlag("tailscale"), formatFlag()}},
			"set": {Args: []func() []string{secretNames}, Flags: []completion.Flag{boolFlag("tailscale"), boolFlag("api-key"),
				boolFlag("dry-run")}},
			"get":           {Args: []func() []string{secretNames}, Flags: []completion.Flag{boolFlag("export"), boolFlag("key")}},
			"delete":        {Args: []func() []string{secretNames}},
			"import":        {Flags: []completion.Flag{boolFlag("yes")}},
			"export":        {Args: []func() []string{secretNames}, Flags: []completion.Flag{flag("out")}},
			"edit":          {},
			"rotate-secret": {Flags: []completion.Flag{boolFlag("force")}},
		}},
		"databricks": {Flags: []completion.Flag{valuedFlag("profile", providerNames)}, Sub: map[string]*completion.Node{
			"check": {Flags: []completion.Flag{boolFlag("all"), valuedFlag("profile", providerNames), boolFlag("1m"), flag("model")}},
			"login": {Args: []func() []string{providerNames}, Flags: []completion.Flag{flag("auth-type"), boolFlag("dry-run"), boolFlag("force"), valuedFlag("as", func() []string { return []string{"user", "sp"} })}},
			"mint":  {Flags: []completion.Flag{valuedFlag("profile", providerNames), boolFlag("dry-run"), boolFlag("copy"), valuedFlag("as", func() []string { return []string{"user", "sp"} })}},
			"import": {Flags: []completion.Flag{flag("dbxcfg"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
				boolFlag("seal-secrets"), boolFlag("apply")}},
			"export": {Flags: []completion.Flag{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
			"config": {Args: []func() []string{func() []string { return []string{"claude", "codex"} }}, Flags: dbxCfgFlags(true)},
			"rescue": {Flags: append(dbxCfgFlags(false), boolFlag("print"))},
		}},
	}
}

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []completion.Flag {
	fs := []completion.Flag{{Name: "profile", Values: providerNames}, {Name: "host"}, {Name: "wire"},
		{Name: "model"}, {Name: "auth"}, {Name: "set"}, {Name: "unset"}}
	if withOut {
		fs = append(fs, completion.Flag{Name: "out"}, completion.Flag{Name: "force", IsBool: true})
	}
	return fs
}

// modelPinAliases offers "alias=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinAliases() []string {
	out := make([]string, 0, len(models.AliasNames)+1)
	for _, s := range models.AliasNames {
		out = append(out, s+"=")
	}
	return append(out, "all=")
}

// providerNames reads the provider registry straight off disk (no network): the same file
// `claude-code configure provider --list`/`switch` already read.
func providerNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}

func secretNames() []string { return providers.SecretNames() }
