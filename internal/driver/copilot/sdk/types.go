package sdk

import "github.com/edotau/claude-code/internal/driver/copilot"

// Options is the SDK-neutral run configuration owned by the driver.
type Options = copilot.Options

// Result is the SDK-neutral completed turn owned by the driver.
type Result = copilot.Result

// Provider is a named upstream in a registry route.
type Provider = copilot.Provider

// RegistryModel is a selectable entry in a registry route.
type RegistryModel = copilot.RegistryModel

// Model is an entry in the Copilot catalog.
type Model = copilot.Model

// MinMultiProviderCLI is the oldest CLI supporting named providers.
const MinMultiProviderCLI = copilot.MinMultiProviderCLI

// MsgMCPName is the harness message MCP server's name.
const MsgMCPName = copilot.MsgMCPName

// WellKnownModel maps a gateway model to Copilot's catalog form.
func WellKnownModel(id string) string { return copilot.WellKnownModel(id) }

// CatalogOptions selects explicit account authentication or the CLI's own login.
func CatalogOptions(cliPath, token string) Options { return copilot.CatalogOptions(cliPath, token) }

func init() {
	copilot.SDKAsk = Ask
	copilot.SDKListModels = ListModels
}
