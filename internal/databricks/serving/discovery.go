package serving

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// Scope separates accounts and routes without persisting tokens or token fingerprints.
type Scope struct {
	Profile      string `json:"profile"`
	CLIProfile   string `json:"cli_profile"`
	Host         string `json:"host"`
	Spelling     string `json:"spelling"`
	SubjectID    string `json:"subject_id"`
	AnthropicURL string `json:"anthropic_url"`
	OpenAIURL    string `json:"openai_url"`
	ResponsesURL string `json:"responses_url"`
}

// Snapshot is a sealed snapshot; CacheHit describes this read and is never persisted.
type Snapshot struct {
	Schema       int                   `json:"schema"`
	Scope        Scope                 `json:"scope"`
	DiscoveredAt time.Time             `json:"discovered_at"`
	Models       []providers.ModelInfo `json:"models"`
	CheckedAt    time.Time             `json:"checked_at,omitzero"`
	Checks       []CheckResult         `json:"checks,omitempty"`
	CacheHit     bool                  `json:"-"`
}

func NewScope(p *providers.Provider, subject string) (Scope, error) {
	if p == nil || p.Preset != providers.PresetDatabricks || p.SpellingRule() != "databricks-serving" ||
		(p.Options.Wire != "" && p.Options.Wire != providers.WireServing) {
		return Scope{}, errors.New("requires a Databricks serving profile; AI Gateway is not supported")
	}
	host := providers.PresetHost(p.Host)
	if host == "" {
		return Scope{}, errors.New("serving discovery requires an HTTPS workspace host")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ":443")
	routes, err := servingClientRoutes(p, host)
	if err != nil {
		return Scope{}, err
	}
	return Scope{Profile: p.Name, CLIProfile: p.CLIProfile, Host: host,
		Spelling: p.SpellingRule(), SubjectID: subject, AnthropicURL: routes[providers.ProtocolAnthropic],
		OpenAIURL: routes[providers.ProtocolOpenAI], ResponsesURL: routes[providers.ProtocolResponses]}, nil
}

// servingClientRoutes preserves resolved bases but rejects routes outside the workspace's serving subtree.
func servingClientRoutes(p *providers.Provider, host string) (map[string]string, error) {
	routes := make(map[string]string)
	for _, dialect := range []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI, providers.ProtocolResponses} {
		base, ok := p.Route(dialect)
		if !ok {
			continue
		}
		u, err := url.Parse(base)
		if err != nil || !servingRouteOK(u, host) {
			return nil, errors.New("resolved client routes must stay under this workspace's HTTPS serving-endpoints path")
		}
		routes[dialect] = base
	}
	return routes, nil
}

// servingRouteOK reports whether u is a plain https URL on host under the serving-endpoints path.
func servingRouteOK(u *url.URL, host string) bool {
	plain := u.Scheme == "https" && u.User == nil && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && u.RawPath == "" && u.Opaque == ""
	return plain && "https://"+strings.TrimSuffix(strings.ToLower(u.Host), ":443") == host && servingPathOK(u.Path)
}

// servingPathOK reports whether p is a clean path at or under /serving-endpoints.
func servingPathOK(p string) bool {
	if path.Clean(p) != p || strings.ContainsAny(p, "\\%") {
		return false
	}
	return p == "/serving-endpoints" || strings.HasPrefix(p, "/serving-endpoints/")
}

// ValidateIdentity validates current authentication, not model permissions; redirects are refused.
func ValidateIdentity(ctx context.Context, p *providers.Provider) (string, error) {
	scope, err := NewScope(p, "")
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
	if err := providers.Authorize(ctx, p, req.Header); err != nil {
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
	return parseIdentity(resp.Body)
}

// parseIdentity reads the bounded SCIM response and returns the active user's id.
func parseIdentity(r io.Reader) (string, error) {
	const limit = 64 << 10
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
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

// DiscoveryKeyPrefix starts every sealed model-discovery secret name.
const DiscoveryKeyPrefix = "model-discovery/v1/"

func servingDiscoveryKey(scope Scope) string {
	data, _ := json.Marshal(scope) // only string fields
	return fmt.Sprintf(DiscoveryKeyPrefix+"%x", sha256.Sum256(data))
}

// SubjectKeyPrefix starts every sealed remembered-subject secret name.
const SubjectKeyPrefix = "model-discovery-subject/v1/"

// subjectKey names p's remembered subject: a hash of its scope minus the subject plus its non-secret auth identity.
func subjectKey(p *providers.Provider) (string, error) {
	scope, err := NewScope(p, "")
	if err != nil {
		return "", err
	}
	a := p.Auth
	// only strings: a changed host, auth type or client id changes the key
	data, _ := json.Marshal([]any{scope, a.Type, a.Header, a.Env, a.File, a.Command, a.Login, a.ClientID, a.SPClientID})
	return fmt.Sprintf(SubjectKeyPrefix+"%x", sha256.Sum256(data)), nil
}

// RecallSubject is the subject last validated for p, read from the vault with no network; a cache-key input only.
func RecallSubject(p *providers.Provider) (subject string, found bool, err error) {
	name, err := subjectKey(p)
	if err != nil {
		return "", false, err
	}
	raw, found, err := vault.ReadSecret(name)
	if err != nil {
		return "", false, errors.New("cannot read or decrypt remembered subject")
	}
	subject = strings.TrimSpace(raw)
	return subject, found && subject != "", nil
}

func Read(scope Scope, refresh bool) (Snapshot, bool, error) {
	if _, _, err := vault.MasterKey(); err != nil {
		return Snapshot{}, false, errors.New("discovery vault is unavailable or locked")
	}
	raw, found, err := vault.ReadSecret(servingDiscoveryKey(scope))
	if err != nil {
		return Snapshot{}, false, errors.New("cannot read or decrypt discovery cache; refresh cannot bypass vault access")
	}
	var snapshot Snapshot
	if found && !validCachedSnapshot(raw, scope, &snapshot) {
		if refresh {
			return Snapshot{}, false, nil // only successfully decrypted, invalid payloads may be replaced
		}
		return Snapshot{}, false, errors.New("invalid discovery cache payload; use --refresh to rediscover")
	}
	snapshot.Checks = SanitizeChecks(snapshot.Checks)
	return snapshot, found, nil
}

// validCachedSnapshot decodes raw into snapshot and reports whether it is a complete schema-1 snapshot for scope.
func validCachedSnapshot(raw string, scope Scope, snapshot *Snapshot) bool {
	return json.Unmarshal([]byte(raw), snapshot) == nil && snapshot.Scope == scope && completeSnapshot(*snapshot)
}

// Discover reads under a subject validated within identityTTL for the credential's own JWT sub, else validates
// first; a forced refresh always validates and never falls back to stale discovery.
func Discover(ctx context.Context, p *providers.Provider, refresh bool) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	subject, err := currentSubject(ctx, p, refresh)
	if err != nil {
		return Snapshot{}, err
	}
	scope, err := NewScope(p, subject)
	if err != nil {
		return Snapshot{}, err
	}
	old, found, err := Read(scope, refresh)
	if err != nil {
		return Snapshot{}, err
	}
	if found && !refresh {
		old.CacheHit = true
		return old, nil
	}
	infos, err := providers.ListModels(ctx, nil, p)
	if err != nil {
		return Snapshot{}, errors.New("serving endpoints discovery failed; no stale cache fallback")
	}
	return Snapshot{Schema: 1, Scope: scope, DiscoveredAt: time.Now().UTC(), Models: infos,
		CheckedAt: old.CheckedAt, Checks: old.Checks}, nil
}

// identityTTL bounds how long a validated subject stands in for a SCIM /Me call.
const identityTTL = 15 * time.Minute

// validated is the sealed sidecar: the SCIM subject validated for the credential whose JWT sub is TokenSub.
type validated struct {
	Subject  string    `json:"subject"`
	TokenSub string    `json:"token_sub"`
	At       time.Time `json:"at"`
}

// currentSubject is the remembered subject when the live credential's JWT sub matches a fresh sidecar, else /Me's.
func currentSubject(ctx context.Context, p *providers.Provider, refresh bool) (string, error) {
	sub := credentialSub(ctx, p)
	name, kerr := validatedKey(p)
	if !refresh && sub != "" && kerr == nil {
		if v, ok := readValidated(name); ok && v.TokenSub == sub {
			return v.Subject, nil
		}
	}
	subject, err := ValidateIdentity(ctx, p)
	if err != nil || sub == "" || kerr != nil || credentialSub(ctx, p) != sub { // remember only a stable identity
		return subject, err
	}
	rememberValidated(p, name, validated{Subject: subject, TokenSub: sub, At: time.Now().UTC()})
	return subject, nil
}

// rememberValidated seals the sidecar like Save seals a snapshot; a lost write costs one /Me call later.
func rememberValidated(p *providers.Provider, name string, v validated) {
	b, _ := json.Marshal(v) // only strings and a time
	_ = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutRecord(name, vault.Record{Sealed: key.Seal(name, string(b)), Kid: key.Kid, Len: len(b),
			Label: "databricks/" + p.Name, SetAt: v.At})
	})
}

// credentialSub is the JWT sub claim of p's credential, "" for an opaque token or a credential error.
func credentialSub(ctx context.Context, p *providers.Provider) string {
	v, err := providers.Credential(ctx, p)
	var claims struct {
		Sub string `json:"sub"`
	}
	if err != nil || !textutil.JWTClaims(v, &claims) {
		return ""
	}
	return strings.TrimSpace(claims.Sub)
}

// validatedKey names p's sidecar beside its remembered subject, under the same scope and auth identity.
func validatedKey(p *providers.Provider) (string, error) {
	name, err := subjectKey(p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(SubjectKeyPrefix+"%x", sha256.Sum256([]byte("validated\x00"+name))), nil
}

// readValidated returns the sidecar under name when it is complete and younger than identityTTL.
func readValidated(name string) (validated, bool) {
	var v validated
	raw, found, err := vault.ReadSecret(name)
	if err != nil || !found || json.Unmarshal([]byte(raw), &v) != nil || v.Subject == "" || v.TokenSub == "" {
		return validated{}, false
	}
	age := time.Since(v.At)
	return v, age >= 0 && age < identityTTL
}

// SanitizeChecks retains only probe measurements, never error bodies or credential diagnostics.
func SanitizeChecks(checks []CheckResult) []CheckResult {
	var out []CheckResult
	for _, c := range checks {
		if c.Status != "ok" && c.Status != "fail" && c.Status != "missing" && c.Status != StatusTransient {
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

// completeSnapshot reports whether snapshot is schema 1 with a discovery time, and a check time when it has checks.
func completeSnapshot(snapshot Snapshot) bool {
	return snapshot.Schema == 1 && !snapshot.DiscoveredAt.IsZero() &&
		(len(snapshot.Checks) == 0 || !snapshot.CheckedAt.IsZero())
}

// Save seals the snapshot; only a provider/profile label and length stay plaintext, and the menu overlay is unchanged.
func Save(p *providers.Provider, snapshot Snapshot) error {
	scope, err := NewScope(p, snapshot.Scope.SubjectID)
	if err != nil {
		return err
	}
	if scope != snapshot.Scope || strings.TrimSpace(scope.SubjectID) == "" || !completeSnapshot(snapshot) {
		return errors.New("invalid discovery snapshot scope or schema")
	}
	snapshot.Checks = SanitizeChecks(snapshot.Checks)
	data, err := json.Marshal(snapshot)
	if err != nil {
		return errors.New("cannot encode discovery snapshot")
	}
	name := servingDiscoveryKey(scope)
	subjectName, err := subjectKey(p)
	if err != nil {
		return err
	}
	label := "databricks/" + scope.Profile
	if err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		if err := tx.PutRecord(subjectName, vault.Record{Sealed: key.Seal(subjectName, scope.SubjectID), Kid: key.Kid,
			Len: len(scope.SubjectID), Label: label, SetAt: time.Now().UTC()}); err != nil {
			return err
		}
		return tx.PutRecord(name, vault.Record{Sealed: key.Seal(name, string(data)), Kid: key.Kid, Len: len(data),
			Label: label, SetAt: snapshot.DiscoveredAt})
	}); err != nil {
		return errors.New("cannot seal discovery cache; check vault access")
	}
	return nil
}

// snapshotTTL is the default life of a sealed snapshot; cacheTTLEnv overrides it.
const (
	snapshotTTL = 24 * time.Hour
	cacheTTLEnv = "HARNESS_MODEL_CACHE_TTL"
)

// CacheTTL is the snapshot life; an unset or invalid HARNESS_MODEL_CACHE_TTL gives the default (invalid warns).
func CacheTTL() time.Duration {
	raw := strings.TrimSpace(cmp.Or(os.Getenv(cacheTTLEnv), providers.CustomEnv()[cacheTTLEnv]))
	if raw == "" {
		return snapshotTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "serving: %s=%q is not a positive Go duration; using %s\n", cacheTTLEnv, raw, snapshotTTL)
		return snapshotTTL
	}
	return d
}

// Recall reads p's snapshot under its remembered subject, locally and unfiltered; ok is false on any miss or error.
func Recall(p *providers.Provider) (Snapshot, bool) {
	subject, found, err := RecallSubject(p)
	if err != nil || !found {
		return Snapshot{}, false
	}
	scope, err := NewScope(p, subject)
	if err != nil {
		return Snapshot{}, false
	}
	snap, found, err := Read(scope, true)
	if err != nil || !found {
		return Snapshot{}, false
	}
	return snap, true
}
