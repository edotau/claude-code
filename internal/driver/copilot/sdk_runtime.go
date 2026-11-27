package copilot

import (
	"context"
	"errors"
)

// SDKAsk is set by internal/driver/copilot/sdk at init; hook binaries leave it nil.
var SDKAsk func(context.Context, Options, string, []string) (Result, error)

// SDKListModels is set by internal/driver/copilot/sdk at init; hook binaries leave it nil.
var SDKListModels func(context.Context, Options) ([]Model, error)

var errNoSDK = errors.New("copilot: SDK runtime is not linked (import internal/driver/copilot/sdk)")
