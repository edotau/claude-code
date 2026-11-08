package gateway

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/router"
)

// Spec describes completion for the gateway command.
func Spec() *completion.Node {
	return &completion.Node{Flags: gatewayRouteFlags(), Sub: map[string]*completion.Node{
		"token":  {},
		"router": {Flags: gatewayRouteFlags()},
		"proxy": {Flags: []completion.Flag{{Name: "start", IsBool: true}, {Name: "stop", IsBool: true}, {Name: "restart", IsBool: true},
			{Name: "status", IsBool: true}, {Name: "serve", IsBool: true}, {Name: "port"}, completion.FormatFlag()}},
	}}
}

func gatewayRouteFlags() []completion.Flag {
	return []completion.Flag{{Name: "model", Values: func() []string { return router.Aliases }},
		{Name: "agent", Values: func() []string { return router.TableAgents }},
		{Name: "task"}, {Name: "fallback", Values: func() []string { return router.FallbackModes }}, completion.FormatFlag()}
}
