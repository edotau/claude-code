package cli

import (
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// gateway router's flags, which bare `gateway` also takes.
func gatewayRouteFlags() []flagInfo {
	flag, valuedFlag, formatFlag := specFlag, specValuedFlag, specFormatFlag
	return []flagInfo{valuedFlag("model", func() []string { return router.Aliases }),
		valuedFlag("agent", func() []string { return router.TableAgents }),
		flag("task"), valuedFlag("fallback", func() []string { return router.FallbackModes }), formatFlag()}
}

func vaultSections() []string {
	return []string{"secrets", "pins", "env", "providers", "agents", "settings", "vault", "all"}
}

func integrationSpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"models": {flags: []flagInfo{valuedFlag("profile", providerNames), boolFlag("unpin"), boolFlag("live"),
			valuedFlag("pin", modelPinAliases), boolFlag("dry-run"), formatFlag()}},
		"gateway": {flags: gatewayRouteFlags(), sub: map[string]*compNode{
			"token":  {},
			"router": {flags: gatewayRouteFlags()},
			"proxy": {flags: []flagInfo{boolFlag("start"), boolFlag("stop"), boolFlag("status"), boolFlag("serve"),
				flag("port"), formatFlag()}},
		}},
		"workflow": {sub: map[string]*compNode{
			"skeleton": {args: []func() []string{workflowPatterns}, flags: []flagInfo{flag("name")}},
			"lint":     {},
		}},
		"review": {sub: reviewSubNodes()},
		"vault": {sub: map[string]*compNode{
			"list": {args: []func() []string{vaultSections}, flags: []flagInfo{boolFlag("tailscale"), formatFlag()}},
			"set": {args: []func() []string{secretNames}, flags: []flagInfo{boolFlag("tailscale"), boolFlag("api-key"),
				boolFlag("dry-run")}},
			"get":           {args: []func() []string{secretNames}, flags: []flagInfo{boolFlag("export"), boolFlag("key")}},
			"delete":        {args: []func() []string{secretNames}},
			"import":        {flags: []flagInfo{boolFlag("yes")}},
			"export":        {args: []func() []string{secretNames}, flags: []flagInfo{flag("out")}},
			"edit":          {},
			"rotate-secret": {flags: []flagInfo{boolFlag("force")}},
		}},
		"databricks": {flags: []flagInfo{valuedFlag("profile", providerNames)}, sub: map[string]*compNode{
			"check": {flags: []flagInfo{boolFlag("all"), valuedFlag("profile", providerNames), boolFlag("1m"), flag("model")}},
			"login": {args: []func() []string{providerNames}, flags: []flagInfo{flag("auth-type"), boolFlag("dry-run"), boolFlag("force"), valuedFlag("as", func() []string { return []string{"user", "sp"} })}},
			"mint":  {flags: []flagInfo{valuedFlag("profile", providerNames), boolFlag("dry-run"), boolFlag("copy"), valuedFlag("as", func() []string { return []string{"user", "sp"} })}},
			"import": {flags: []flagInfo{flag("dbxcfg"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
				boolFlag("seal-secrets"), boolFlag("apply")}},
			"export": {flags: []flagInfo{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
			"config": {args: []func() []string{func() []string { return []string{"claude", "codex"} }}, flags: dbxCfgFlags(true)},
			"rescue": {flags: append(dbxCfgFlags(false), boolFlag("print"))},
		}},
	}
}

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []flagInfo {
	fs := []flagInfo{{name: "profile", values: providerNames}, {name: "host"}, {name: "wire"},
		{name: "model"}, {name: "auth"}, {name: "set"}, {name: "unset"}}
	if withOut {
		fs = append(fs, flagInfo{name: "out"}, flagInfo{name: "force", isBool: true})
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
