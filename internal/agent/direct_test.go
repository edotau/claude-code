package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// wireSpy is a Driver whose Wire records that configureWire got that far.
type wireSpy struct {
	Driver
	wired bool
}

func (w *wireSpy) Wire(context.Context, WireRoute, *Headless) ([]string, []string, error) {
	w.wired = true
	return nil, nil, nil
}

// unsignedJWT is a JWT-shaped credential whose exp claim is left in the future.
func unsignedJWT(left time.Duration) string {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{"alg":"none"}`) + "." + seg(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(left).Unix())) + ".sig"
}

// sealM2M stores an m2m row's client secret and its sealed access slot, as a prior mint leaves them.
func sealM2M(t *testing.T, p *providers.Provider, access string) {
	t.Helper()
	name := providers.AuthSecretName(p)
	sum := sha256.Sum256([]byte(providers.AuthHost(p) + "\x00" + p.Auth.ClientID + "\x00" + "s3cret"))
	slot, err := json.Marshal(map[string]string{"access": access, "bind": hex.EncodeToString(sum[:8])})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{name: "s3cret", name + "_ACCESS": string(slot)} {
		if err := vault.Store(k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// A direct run is never refused up front, even when its deadline outlasts the sealed token's remaining life.
func TestConfigureWireDirectNeverRefusesOnTokenLife(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "direct-life-m2m", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: "https://dbc-x.cloud.databricks.com/serving-endpoints/anthropic",
		Auth:    providers.Auth{Type: providers.AuthM2M, ClientID: "cid"}}
	sealM2M(t, p, unsignedJWT(30*time.Minute))
	ctx, cancel := context.WithTimeout(WithDirect(t.Context()), 45*time.Minute)
	defer cancel()
	spy := &wireSpy{}
	if _, _, err := (Schema{Name: "codex"}).configureWire(ctx, spy, providers.Target{Provider: p, Model: "m"}, nil); err != nil || !spy.wired {
		t.Fatalf("err=%v wired=%v, want a wired direct run", err, spy.wired)
	}
}

// A direct run whose auth fails after output started names the static token and the routed default; a routed run does not.
func TestRunDirectAuthFailureNamesRoutedDefault(t *testing.T) {
	setConfigDir(t)
	for _, direct := range []bool{true, false} {
		f := &fakeRunner{caps: Caps{Direct: Native}, write: "partial", errs: []error{errors.New("HTTP 401 unauthorized")}}
		_, err := Run(context.Background(), f, RunOptions{Prompt: "p", Direct: direct}, wire.Discard, nil)
		if err == nil {
			t.Fatalf("direct=%v: want an error", direct)
		}
		if got := strings.Contains(err.Error(), "drop --direct"); got != direct {
			t.Errorf("direct=%v: hint present = %v: %v", direct, got, err)
		}
	}
}
