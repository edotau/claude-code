package network

import (
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Spec is the completion node for the network verb.
func Spec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	valuedFlag, formatFlag := helptext.ValuedFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"restart": {Flags: []helptext.Flag{boolFlag("dry-run"), boolFlag("force"), formatFlag()}},
		"tailscale": {Flags: []helptext.Flag{formatFlag(), boolFlag("machines"), boolFlag("connect"), boolFlag("key"),
			valuedFlag("exit-node", exitNodeActions), valuedFlag("subnet", subnetActions),
			valuedFlag("route", routeTargets), flag("auth-key"),
			flag("key-env"), boolFlag("advertise-exit"),
			flag("expiry"), flag("tags"), boolFlag("save"), boolFlag("reusable"), boolFlag("ephemeral"),
			boolFlag("preauthorized"), flag("description")}},
		"browser": {Flags: []helptext.Flag{formatFlag(), boolFlag("setup"), valuedFlag("browser", browserNames),
			boolFlag("dry-run")}},
		"websearch": {Flags: []helptext.Flag{flag("query"), flag("max-results"), flag("extract")}},
		"remote": {Flags: []helptext.Flag{valuedFlag("host", tailscale.CachedMachineNames), flag("dir"),
			valuedFlag("agent", func() []string { return remote.Agents }), flag("name"), boolFlag("dry-run"),
			boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
	}}
}

// exitNodeActions are the `network tailscale --exit-node` values.
func exitNodeActions() []string { return []string{"on", "off", "approve"} }

// subnetActions are the `network tailscale --subnet` keyword values (a CIDR list is also accepted).
func subnetActions() []string { return []string{"approve", "off"} }

// routeTargets are the `network tailscale --route` values: dispatch hosts or "off" to clear.
func routeTargets() []string { return append(tailscale.CachedMachineNames(), "off") }

// browserNames mirrors `network browser --setup --browser`'s accepted values.
func browserNames() []string {
	return []string{"chrome", "edge", "brave", "arc", "vivaldi", "chromium", "default"}
}
