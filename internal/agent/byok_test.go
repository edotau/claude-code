package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

func TestByokRetryModelSkipsFailedAndStaysOnPool(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "byok-models", `["m-a","m-b"]`) }); err != nil {
		t.Fatal(err)
	}
	p := &providers.Provider{Name: "dbx", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "X"}}
	first, note := router.PinnedModel("opencode", router.Aliases[1], router.Cell{}, p, router.BYOKModel)
	if first != "m-a" || note != "" {
		t.Fatalf("first pick = %q (%q), want m-a", first, note)
	}
	disp := Dispatch{repick: byokRepick("opencode", router.Aliases[1], router.Cell{}, p, "dbx:")}
	capErr := errors.New("400: max_new_tokens exceeds the model limit")
	if got := retryModel(disp, RunOptions{Model: "dbx:m-a"}, errors.New("connection reset")); got != "" {
		t.Errorf("non-capability error retried on %q", got)
	}
	if got := retryModel(disp, RunOptions{Model: "dbx:m-a"}, capErr); got != "dbx:m-b" {
		t.Errorf("capability retry = %q, want dbx:m-b", got)
	}
	schemaErr := errors.New(`Bad Request: Invalid JSON payload received. Unknown name "$schema" at 'tools[0].function_declarations[0].parameters'`)
	if got := retryModel(disp, RunOptions{Model: "dbx:m-b"}, schemaErr); got != "" {
		t.Errorf("tool-schema retry with the pool exhausted = %q, want none", got)
	}
	assertByokSkippedModels(t, p.Name)
	if got := retryModel(Dispatch{}, RunOptions{Model: "x"}, capErr); got != "" {
		t.Errorf("dispatch without a byok repick retried on %q", got)
	}
}

func assertByokSkippedModels(t *testing.T, provider string) {
	t.Helper()
	if router.LoadState()[router.ProviderAccount(provider)].BYOKSkip["opencode|m-b"].IsZero() {
		t.Error("tool-schema rejection did not mark m-b skipped for opencode")
	}
	at := router.LoadState()[router.ProviderAccount(provider)].BYOKSkip["opencode|m-a"]
	if time.Since(at) > time.Minute || at.IsZero() {
		t.Errorf("m-a not marked skipped for opencode: %v", at)
	}
}

// ENDPOINT_NOT_FOUND marks the model gone and returns the next READY Responses model.
func TestServedRepickOnEndpointNotFound(t *testing.T) {
	quietStderr(t)
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	disp := Dispatch{servedRepick: routeCtx{reg: reg}.servedRepickFor(servedCand)}
	if disp.servedRepick == nil {
		t.Fatal("codex on a Databricks profile with the switch on must get a repick")
	}
	err := errors.New("RESOURCE_DOES_NOT_EXIST: ENDPOINT_NOT_FOUND: no endpoint m-a")
	if got := retryModel(disp, RunOptions{Model: "dev:m-a"}, err); got != "dev:m-b" {
		t.Fatalf("repick = %q, want dev:m-b", got)
	}
	if at := router.LoadState()[router.ProviderAccount("dev")].Unserved["m-a"]; at.IsZero() {
		t.Error("m-a was not marked unserved")
	}
	if got := retryModel(disp, RunOptions{Model: "dev:m-b"}, err); got != "" {
		t.Errorf("second failure repicked %q, want none left", got)
	}
}

// an error that is not an endpoint-gone answer neither repicks nor marks.
func TestServedRepickIgnoresUnrelatedError(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	disp := Dispatch{servedRepick: routeCtx{reg: reg}.servedRepickFor(servedCand)}
	if got := retryModel(disp, RunOptions{Model: "dev:m-a"}, errors.New("connection reset")); got != "" {
		t.Errorf("unrelated error repicked %q", got)
	}
	if at := router.LoadState()[router.ProviderAccount("dev")].Unserved["m-a"]; !at.IsZero() {
		t.Error("an unrelated error must not mark m-a")
	}
}

// an exact --model, a non-codex dispatch and a switch-off run get no served repick.
func TestServedRepickNotOfferedWhereItWouldMislead(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	if (routeCtx{reg: reg, exact: "m-a"}.servedRepickFor(servedCand)) != nil {
		t.Error("exact --model must not repick")
	}
	copilot := servedCand
	copilot.Agent = "copilot"
	if (routeCtx{reg: reg}.servedRepickFor(copilot)) != nil {
		t.Error("a non-codex dispatch must not get a served repick")
	}
	off := servedEnv(t, "false", nil)
	if (routeCtx{reg: off}.servedRepickFor(servedCand)) != nil {
		t.Error("switch off must not repick")
	}
}

// with both repicks set, a capability error still takes the byok pool path.
func TestByokRetryModelKeepsCapabilityPathWithServedRepick(t *testing.T) {
	disp := Dispatch{
		repick:       func(string) string { return "dev:pool-next" },
		servedRepick: func(string) string { return "dev:served-next" },
	}
	if got := retryModel(disp, RunOptions{Model: "dev:m"}, errors.New("400: max_new_tokens too large")); got != "dev:pool-next" {
		t.Errorf("capability error = %q, want the byok pick", got)
	}
}

// A Progressed failure never repicks: the next model would re-run the prompt and repeat its tools.
func TestRetryModelSkipsProgressedError(t *testing.T) {
	disp := Dispatch{repick: func(string) string { return "dbx:m-b" }}
	capMsg := errors.New("400: max_new_tokens exceeds the model limit")
	if got := retryModel(disp, RunOptions{Model: "dbx:m-a"}, &Error{Kind: KindFatal, Agent: "x", Progressed: true, Err: capMsg}); got != "" {
		t.Errorf("progressed capability error repicked %q, want none", got)
	}
	if got := retryModel(disp, RunOptions{Model: "dbx:m-a"}, &Error{Kind: KindFatal, Agent: "x", Err: capMsg}); got != "dbx:m-b" {
		t.Errorf("unprogressed capability error = %q, want dbx:m-b", got)
	}
}
