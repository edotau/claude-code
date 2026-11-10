package network

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/network/tailscale"
)

// Spec is the completion node for the network verb.
func Spec() *completion.Node {
	flag, boolFlag, valuedFlag, formatFlag := completion.Plain, completion.BoolFlag, completion.ValuedFlag, completion.FormatFlag
	return &completion.Node{Sub: map[string]*completion.Node{
		"restart": {Flags: []completion.Flag{boolFlag("dry-run"), boolFlag("force"), formatFlag()}},
		"tailscale": {Flags: []completion.Flag{formatFlag(), boolFlag("machines"), boolFlag("connect"), boolFlag("key"),
			valuedFlag("exit-node", exitNodeActions), valuedFlag("subnet", subnetActions), valuedFlag("route", routeTargets), flag("auth-key"),
			flag("key-env"), boolFlag("advertise-exit"),
			flag("expiry"), flag("tags"), boolFlag("save"), boolFlag("reusable"), boolFlag("ephemeral"),
			boolFlag("preauthorized"), flag("description")}},
		"browser":   {Flags: []completion.Flag{formatFlag(), boolFlag("setup"), valuedFlag("browser", browserNames), boolFlag("dry-run")}},
		"websearch": {Flags: []completion.Flag{flag("query"), flag("max-results"), flag("extract")}},
		"remote": {Flags: []completion.Flag{valuedFlag("host", tailscale.CachedMachineNames), flag("dir"), valuedFlag("agent", func() []string { return remote.RemoteAgents }),
			flag("name"), boolFlag("dry-run"), boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
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
