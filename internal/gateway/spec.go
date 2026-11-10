package gateway

import (
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Spec describes completion for the gateway command.
func Spec() *helptext.Node {
	return &helptext.Node{Flags: gatewayRouteFlags(), Sub: map[string]*helptext.Node{
		"proxy": {Flags: []helptext.Flag{{Name: "start", IsBool: true}, {Name: "stop", IsBool: true}, {Name: "restart", IsBool: true},
			{Name: "status", IsBool: true}, {Name: "serve", IsBool: true}, {Name: "port"}, helptext.CompletionFormatFlag()}},
	}}
}

func gatewayRouteFlags() []helptext.Flag {
	return []helptext.Flag{{Name: "model", Values: func() []string { return router.Aliases }},
		{Name: "agent", Values: func() []string { return router.TableAgents }},
		{Name: "task"}, {Name: "fallback", Values: func() []string { return router.FallbackModes }}, helptext.CompletionFormatFlag()}
}
