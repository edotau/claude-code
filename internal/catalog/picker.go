package catalog

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// IsGithubProfile is true for a registry row whose models come from the Copilot catalog, not a gateway listing.
func IsGithubProfile(p *providers.Provider) bool { return p != nil && p.Provider == "github" }

// ProfileCatalogSupport requires an explicit account for discovery without launch-account fallback.
func ProfileCatalogSupport(p *providers.Provider) string {
	if strings.TrimSpace(p.Login) == "" {
		return "Copilot discovery requires an explicit profile login; implicit launch-account fallback is disabled"
	}
	return ""
}

// discoverySource is where p's models are discovered from and, when discovery cannot run, why not.
func discoverySource(p *providers.Provider) (source, skip string) {
	source, skip = providers.DiscoverySource(p)
	if IsGithubProfile(p) {
		skip = ProfileCatalogSupport(p)
	}
	if source == "" && skip != "" {
		skip += "; choose its models in the vendor CLI itself"
	}
	return source, skip
}

// GithubLister lists a github row's live models; the copilot package supplies it.
type GithubLister func(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error)

// discoverGateway is the gateway listing call; tests swap it for a canned listing.
var discoverGateway = providers.DiscoverModels

// DiscoverResult is one profile's `models discover` outcome.
type DiscoverResult struct {
	Provider     string                `json:"provider"`
	Profile      string                `json:"profile"`
	Source       string                `json:"source"`
	Discovered   int                   `json:"discovered"`
	Additions    []providers.ModelInfo `json:"additions"`
	Dropped      []string              `json:"skipped_dropped,omitempty"`
	Deprecated   []string              `json:"dropped_deprecated,omitempty"`
	ProbeNote    string                `json:"probe_note,omitempty"`
	Destinations []string              `json:"destinations"`
	Status       string                `json:"status"`
	Note         string                `json:"note,omitempty"`
}

// missingKeyNote explains a key profile whose key is absent or unreadable ("" when it resolves or is no key row).
func missingKeyNote(ctx context.Context, p *providers.Provider) string {
	if p.Auth.Type != providers.AuthKey {
		return ""
	}
	if _, err := providers.Credential(ctx, p); err == nil {
		return ""
	}
	name := cmp.Or(providers.SecretName(p.Provider, p.Name, providers.AuthKey), p.Auth.Env)
	if _, err := vault.HasSecretErr(name); err != nil || providers.CredentialPresent(p) {
		return "this profile's API key could not be read; check the vault with `claude-code doctor`"
	}
	return "no API key stored for this profile; add one with `claude-code vault set " + name + "`"
}

// discoverUsable is p's live listing, or a skip note when p has no API key or its account lists nothing.
func discoverUsable(ctx context.Context, p *providers.Provider, github GithubLister) (live []providers.ModelInfo, note string, err error) {
	if note = missingKeyNote(ctx, p); note != "" {
		return nil, note, nil
	}
	list := discoverGateway
	if IsGithubProfile(p) {
		list = github
	}
	if live, err = list(ctx, p); err == nil && len(live) == 0 {
		note = "the catalog listed no models for this account; " + emptyCatalogStep(p)
	}
	return live, note, err
}

// emptyCatalogStep is what to check when p's live catalog lists nothing.
func emptyCatalogStep(p *providers.Provider) string {
	if IsGithubProfile(p) {
		return "check `gh auth status` and that " + cmp.Or(p.Login, "the account") + " holds a Copilot seat"
	}
	return "check the profile's credential with `claude-code doctor`"
}

// discoverPickerProfile live-discovers p's models and previews (or, with apply, appends) the saved picker additions.
func discoverPickerProfile(p *providers.Provider, github GithubLister, apply, verify bool) DiscoverResult {
	r := DiscoverResult{Provider: p.Provider, Profile: p.Name, Additions: []providers.ModelInfo{}, Destinations: []string{}, Status: "skipped"}
	source, skip := discoverySource(p)
	r.Source = source
	if skip != "" {
		r.Note = skip
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	live, note, err := discoverUsable(ctx, p, github)
	if err != nil {
		r.Status = "error"
		r.Note = err.Error() + " (no fallback saved)"
		return r
	}
	if note != "" {
		r.Note = note
		return r
	}
	r.Discovered = len(live)
	r.Destinations = discoverDestinations(p)
	if r.Dropped, err = providers.DroppedLive(p, live); err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	r.Dropped = withReasons(p, r.Dropped)
	r.Deprecated, r.ProbeNote = databricksListing(ctx, p, live, apply, verify)
	var added []providers.ModelInfo
	if apply {
		added, err = providers.AppendPickerModels(p, live)
	} else {
		added, err = providers.PreviewPickerModels(p, live)
	}
	if err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	if len(added) > 0 {
		r.Additions = added
	}
	r.Status = "preview"
	if apply {
		r.Status = "applied"
	}
	r.Note = "Listing is not inference proof; existing selections and native vendor catalogs are unchanged."
	return r
}

func discoverDestinations(p *providers.Provider) []string {
	if p.Provider == "github" {
		return []string{"sealed profile history only (Copilot pickers retain live policy filtering)"}
	}
	d := []string{"configure model picker", "agent model picker", "OpenCode harness-generated catalog (next routed launch)"}
	if p.Preset == providers.PresetDatabricks {
		d = append(d, "Codex generated catalog (Responses models with bundled metadata only)")
	}
	return d
}

// DiscoverPickerProfiles runs discoverPickerProfile over names, listing github rows with github; ok is false when one errored or none completed.
func DiscoverPickerProfiles(reg *providers.Registry, names []string, github GithubLister, apply, verify bool) (results []DiscoverResult, ok bool) {
	results = make([]DiscoverResult, 0, len(names))
	ok = true
	completed := 0
	for _, name := range names {
		p, err := reg.Get(name)
		if err != nil {
			ok = false
			results = append(results, DiscoverResult{Profile: name, Additions: []providers.ModelInfo{}, Destinations: []string{},
				Status: "error", Note: err.Error()})
			continue
		}
		r := discoverPickerProfile(p, github, apply, verify)
		if r.Status == "error" {
			ok = false
		}
		if r.Status == "applied" || r.Status == "preview" {
			completed++
		}
		results = append(results, r)
	}
	return results, ok && completed > 0
}

// DropResult is one profile's `models discover --drop|--undrop` outcome; Profile "" is the global saved menu.
type DropResult struct {
	Profile string   `json:"profile"`
	Removed []string `json:"removed"`
	Error   string   `json:"error,omitempty"`
}

// DropPickerProfiles drops (or un-drops) ids from the global menu (when global) and each named profile's picker.
// write false previews. ok is false when any profile errored.
func DropPickerProfiles(reg *providers.Registry, names, ids []string, undrop, global, write bool) (out []DropResult, ok bool) {
	ok = true
	if global && !undrop {
		r := DropResult{Removed: []string{}}
		removed, err := providers.DropFromMenu(ids, write)
		r.Removed = append(r.Removed, removed...)
		if err != nil {
			r.Error, ok = err.Error(), false
		}
		out = append(out, r)
	}
	for _, name := range names {
		r := DropResult{Profile: name, Removed: []string{}}
		p, err := reg.Get(name)
		if err == nil {
			var removed []string
			removed, err = providers.EditPickerDrops(p, ids, undrop, write)
			r.Removed = append(r.Removed, removed...)
		}
		if err != nil {
			r.Error, ok = err.Error(), false
		}
		out = append(out, r)
	}
	return out, ok
}

// probeEndpoints is the deprecation probe's HTTP call; tests swap it for canned replies.
var probeEndpoints = serving.Probe

// databricksListing keeps an applied listing's api_types and limits, then drops the ids the probe finds deprecated.
func databricksListing(ctx context.Context, p *providers.Provider, live []providers.ModelInfo, apply, verify bool) (dropped []string, note string) {
	if p.Preset != providers.PresetDatabricks {
		return nil, ""
	}
	if !apply {
		return nil, "no deprecation probe sent in a preview; --apply probes new ids (--verify also the saved ones)"
	}
	if err := router.WarmServed(p.Name, live, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s served listing not saved: %v\n", p.Name, err)
	}
	ids, err := probeCandidates(p, live, verify)
	if err != nil {
		return nil, "deprecation probe skipped: " + err.Error()
	}
	dropped, broken, err := deprecatedIDs(ctx, p, live, ids)
	for i := 0; err == nil && i < len(dropped); i++ {
		_, err = providers.RecordDeprecated(p, dropped[i], time.Now())
	}
	if err != nil {
		return nil, "deprecation probe changed nothing: " + err.Error()
	}
	return dropped, brokenNote(broken)
}

// withReasons appends each id's recorded drop reason, e.g. "gpt-5 (deprecated 2026-11-20)".
func withReasons(p *providers.Provider, ids []string) []string {
	why, _ := providers.DroppedReasons(p)
	out := slices.Clone(ids)
	for i, id := range out {
		if w := why[id]; w != "" {
			out[i] = id + " (" + w + ")"
		}
	}
	return out
}

// brokenNote names each id whose stream broke; none is dropped, the owner decides.
func brokenNote(broken []string) string {
	lines := make([]string, len(broken))
	for i, line := range broken {
		id, _, _ := strings.Cut(line, ":")
		lines[i] = fmt.Sprintf("stream check failed, not dropped: %s; to drop it: claude-code models discover --drop %s --apply", line, id)
	}
	return strings.Join(lines, "\n  ")
}

// probeCandidates are the ids discovery would add, plus p's saved additions when verify is set.
func probeCandidates(p *providers.Provider, live []providers.ModelInfo, verify bool) ([]string, error) {
	added, err := providers.PreviewPickerModels(p, live)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range added {
		ids = append(ids, m.ID)
	}
	if verify {
		saved, err := providers.PickerModels(p)
		if err != nil {
			return nil, err
		}
		for _, m := range saved {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// deprecatedIDs sends ids one request on the route each speaks and returns those answered 400 "This endpoint <id> is deprecated".
func deprecatedIDs(ctx context.Context, p *providers.Provider, live []providers.ModelInfo, ids []string) (dropped, broken []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	byID := make(map[string]providers.ModelInfo, len(live))
	for _, m := range live {
		byID[m.ID] = m
	}
	targets := make([]serving.CheckTarget, len(ids))
	for i, id := range ids {
		m := byID[id]
		resp := slices.Contains(m.APITypes, "openai/v1/responses")
		targets[i] = serving.CheckTarget{ID: id, Wire: id, Responses: resp, NoStream: !resp && m.Task != "" && m.Task != "llm/v1/chat"}
	}
	// the listing's 45 s deadline would cut a 30 s-per-request probe round short
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	results, err := probeEndpoints(ctx, p, targets, serving.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: 30 * time.Second})
	if err != nil {
		return nil, nil, err
	}
	dropped, broken = probeVerdicts(results)
	return dropped, broken, nil
}

func deprecatedAnswer(r serving.CheckResult) bool {
	id, ok := serving.DeprecatedEndpoint(r.HTTP, r.Error)
	return ok && id == r.ID
}

// probeVerdicts splits results into deprecated ids and "id: reason" lines for streams that broke.
func probeVerdicts(results []serving.CheckResult) (dropped, broken []string) {
	for _, r := range results {
		switch {
		case deprecatedAnswer(r):
			dropped = append(dropped, r.ID)
		case r.Status == serving.StatusBroken:
			broken = append(broken, r.ID+": "+r.Error)
		}
	}
	return dropped, broken
}
