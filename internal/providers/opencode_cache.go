package providers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

// ServingDiscoveryScope separates accounts and routes without persisting tokens or token fingerprints.
type ServingDiscoveryScope struct {
	Profile      string `json:"profile"`
	CLIProfile   string `json:"cli_profile"`
	Host         string `json:"host"`
	Spelling     string `json:"spelling"`
	SubjectID    string `json:"subject_id"`
	AnthropicURL string `json:"anthropic_url"`
	OpenAIURL    string `json:"openai_url"`
	ResponsesURL string `json:"responses_url"`
}

// ServingDiscovery is a sealed snapshot; CacheHit describes this read and is never persisted.
type ServingDiscovery struct {
	Schema       int                   `json:"schema"`
	Scope        ServingDiscoveryScope `json:"scope"`
	DiscoveredAt time.Time             `json:"discovered_at"`
	Models       []ModelInfo           `json:"models"`
	CheckedAt    time.Time             `json:"checked_at,omitzero"`
	Checks       []CheckResult         `json:"checks,omitempty"`
	CacheHit     bool                  `json:"-"`
}

func servingDiscoveryScope(p *Provider, subject string) (ServingDiscoveryScope, error) {
	if p == nil || p.Preset != PresetDatabricks || p.SpellingRule() != "databricks-serving" ||
		(p.Options.Wire != "" && p.Options.Wire != WireServing) {
		return ServingDiscoveryScope{}, errors.New("requires a Databricks serving profile; AI Gateway is not supported")
	}
	host := presetHost(p.Host)
	if host == "" {
		return ServingDiscoveryScope{}, errors.New("serving discovery requires an HTTPS workspace host")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ":443")
	routes, err := servingClientRoutes(p, host)
	if err != nil {
		return ServingDiscoveryScope{}, err
	}
	return ServingDiscoveryScope{Profile: p.Name, CLIProfile: p.CLIProfile, Host: host,
		Spelling: p.SpellingRule(), SubjectID: subject, AnthropicURL: routes[ProtocolAnthropic],
		OpenAIURL: routes[ProtocolOpenAI], ResponsesURL: routes[ProtocolResponses]}, nil
}

// servingClientRoutes preserves resolved bases but rejects routes outside the workspace's serving subtree.
func servingClientRoutes(p *Provider, host string) (map[string]string, error) {
	routes := make(map[string]string)
	for _, dialect := range []string{ProtocolAnthropic, ProtocolOpenAI, ProtocolResponses} {
		base, ok := p.Route(dialect)
		if !ok {
			continue
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
			u.Fragment != "" || u.RawPath != "" || u.Opaque != "" ||
			"https://"+strings.TrimSuffix(strings.ToLower(u.Host), ":443") != host ||
			path.Clean(u.Path) != u.Path || strings.ContainsAny(u.Path, "\\%") ||
			(u.Path != "/serving-endpoints" && !strings.HasPrefix(u.Path, "/serving-endpoints/")) {
			return nil, errors.New("resolved client routes must stay under this workspace's HTTPS serving-endpoints path")
		}
		routes[dialect] = base
	}
	return routes, nil
}

// ValidateDatabricksIdentity validates current authentication, not model permissions; redirects are refused.
func ValidateDatabricksIdentity(ctx context.Context, p *Provider) (string, error) {
	scope, err := servingDiscoveryScope(p, "")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		scope.Host+"/api/2.0/preview/scim/v2/Me?attributes=id,active", nil)
	if err != nil {
		return "", errors.New("cannot construct identity validation request")
	}
	if err := Authorize(ctx, p, req.Header); err != nil {
		return "", errors.New("identity authentication failed; check profile credentials")
	}
	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("identity validation request failed; check authentication and connectivity")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("identity validation failed (HTTP %d); no cached discovery used", resp.StatusCode)
	}
	const limit = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return "", errors.New("cannot read bounded identity response")
	}
	var identity struct {
		ID     string `json:"id"`
		Active *bool  `json:"active"`
	}
	if json.Unmarshal(body, &identity) != nil || strings.TrimSpace(identity.ID) == "" ||
		(identity.Active != nil && !*identity.Active) {
		return "", errors.New("identity response is invalid or inactive; no cached discovery used")
	}
	return identity.ID, nil
}

func servingDiscoveryKey(scope ServingDiscoveryScope) string {
	data, _ := json.Marshal(scope) // only string fields
	return fmt.Sprintf("model-discovery/v1/%x", sha256.Sum256(data))
}

func readServingDiscovery(scope ServingDiscoveryScope, refresh bool) (ServingDiscovery, bool, error) {
	if _, _, err := vault.MasterKey(); err != nil {
		return ServingDiscovery{}, false, errors.New("discovery vault is unavailable or locked")
	}
	raw, found, err := vault.ReadSecret(servingDiscoveryKey(scope))
	if err != nil {
		return ServingDiscovery{}, false, errors.New("cannot read or decrypt discovery cache; refresh cannot bypass vault access")
	}
	var snapshot ServingDiscovery
	if found && (json.Unmarshal([]byte(raw), &snapshot) != nil || snapshot.Schema != 1 || snapshot.Scope != scope ||
		snapshot.DiscoveredAt.IsZero() || (len(snapshot.Checks) > 0 && snapshot.CheckedAt.IsZero())) {
		if refresh {
			return ServingDiscovery{}, false, nil // only successfully decrypted, invalid payloads may be replaced
		}
		return ServingDiscovery{}, false, errors.New("invalid discovery cache payload; use --refresh to rediscover")
	}
	snapshot.Checks = SanitizeServingChecks(snapshot.Checks)
	return snapshot, found, nil
}

// DiscoverServingModels authenticates every read; a forced refresh never falls back to stale discovery.
func DiscoverServingModels(ctx context.Context, p *Provider, refresh bool) (ServingDiscovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	subject, err := ValidateDatabricksIdentity(ctx, p)
	if err != nil {
		return ServingDiscovery{}, err
	}
	scope, err := servingDiscoveryScope(p, subject)
	if err != nil {
		return ServingDiscovery{}, err
	}
	old, found, err := readServingDiscovery(scope, refresh)
	if err != nil {
		return ServingDiscovery{}, err
	}
	if found && !refresh {
		old.CacheHit = true
		return old, nil
	}
	infos, err := ListModels(ctx, nil, p)
	if err != nil {
		return ServingDiscovery{}, errors.New("Serving Endpoints discovery failed; no stale cache fallback")
	}
	return ServingDiscovery{Schema: 1, Scope: scope, DiscoveredAt: time.Now().UTC(), Models: infos,
		CheckedAt: old.CheckedAt, Checks: old.Checks}, nil
}

// SanitizeServingChecks retains only probe measurements, never error bodies or credential diagnostics.
func SanitizeServingChecks(checks []CheckResult) []CheckResult {
	var out []CheckResult
	for _, c := range checks {
		if c.Status != "ok" && c.Status != "fail" && c.Status != "missing" {
			continue
		}
		tools := ""
		if c.Tools == "yes" || c.Tools == "no" {
			tools = c.Tools
		}
		out = append(out, CheckResult{ID: c.ID, Status: c.Status, HTTP: c.HTTP, LatencyMS: c.LatencyMS, Tools: tools})
	}
	return out
}

// SaveServingDiscovery seals the snapshot with empty plaintext Prefix metadata; it does not change the menu overlay.
func SaveServingDiscovery(p *Provider, snapshot ServingDiscovery) error {
	scope, err := servingDiscoveryScope(p, snapshot.Scope.SubjectID)
	if err != nil {
		return err
	}
	if scope != snapshot.Scope || strings.TrimSpace(scope.SubjectID) == "" || snapshot.Schema != 1 ||
		snapshot.DiscoveredAt.IsZero() || (len(snapshot.Checks) > 0 && snapshot.CheckedAt.IsZero()) {
		return errors.New("invalid discovery snapshot scope or schema")
	}
	snapshot.Checks = SanitizeServingChecks(snapshot.Checks)
	data, err := json.Marshal(snapshot)
	if err != nil {
		return errors.New("cannot encode discovery snapshot")
	}
	name := servingDiscoveryKey(scope)
	if err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutRecord(name, vault.Record{Sealed: key.Seal(name, string(data)), Kid: key.Kid, SetAt: time.Now().UTC()})
	}); err != nil {
		return errors.New("cannot seal discovery cache; check vault access")
	}
	return nil
}
