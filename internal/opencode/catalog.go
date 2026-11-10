package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeProvider resolves the workspace profile: --profile, else the opencode pin, else the session profile.
func opencodeProvider(name string) (*providers.Provider, error) {
	p, err := catalog.Profile("opencode", name)
	if err == nil && p == nil {
		err = errors.New("no profile pinned for opencode; pass --profile")
	}
	return p, err
}

// OpencodeServed lists the workspace's endpoints by name.
func OpencodeServed(name string) (map[string]providers.ModelInfo, *providers.Provider, error) {
	p, err := opencodeProvider(name)
	if err != nil {
		return nil, nil, err
	}
	served, err := catalog.Served(p)
	return served, p, err
}

// OpencodeSyncModels resolves the profile; discovery validates identity before reading the sealed cache.
func OpencodeSyncModels(profile string, refresh bool) (*providers.Provider, serving.Snapshot, error) {
	p, err := opencodeProvider(profile)
	if errors.Is(err, catalog.ErrGateway) {
		return nil, serving.Snapshot{}, err
	}
	if err != nil {
		return nil, serving.Snapshot{}, errors.New("cannot resolve profile; check the provider configuration")
	}
	snapshot, err := serving.Discover(context.Background(), p, refresh)
	return p, snapshot, err
}

// OpencodeSyncIDs selects unique, losslessly spellable READY chat endpoint ids in deterministic order.
func OpencodeSyncIDs(infos []providers.ModelInfo) []string {
	var ids []string
	for _, info := range infos {
		if info.Gateway || info.Ready != "READY" || info.Task != "llm/v1/chat" ||
			!strings.HasPrefix(info.ID, models.DatabricksPrefix) {
			continue
		}
		id := catalog.ID(info.ID)
		if id != "" && models.Wire("databricks-serving", id) == info.ID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func OpencodeSyncMerge(menu []providers.OpencodeModel, ids []string, checks []serving.CheckResult) ([]providers.OpencodeModel, int) {
	failed := make(map[string]bool)
	for _, c := range checks {
		failed[c.ID] = c.Status == "fail" || c.Status == "missing"
	}
	next := slices.Clone(menu)
	skipped := 0
	for _, id := range ids {
		if !slices.ContainsFunc(menu, func(m providers.OpencodeModel) bool { return m.ID == id }) {
			if failed[id] {
				skipped++
				continue
			}
			// MenuAdd canonicalizes its input, so pass the lossless wire name rather than canonicalizing the id twice.
			next = MenuAdd(next, models.Wire("databricks-serving", id), "", nil)
		}
	}
	return next, skipped
}

func OpencodeSyncVerify(p *providers.Provider, snapshot *serving.Snapshot, ids []string, menu []providers.OpencodeModel) error {
	responses := make(map[string]bool, len(menu))
	for _, m := range menu {
		responses[m.ID] = m.Responses
	}
	targets := make([]serving.CheckTarget, 0, len(ids))
	for _, id := range ids {
		r, exists := responses[id]
		if !exists {
			r = models.ResponsesOnly(id)
		}
		targets = append(targets, serving.CheckTarget{ID: id, Wire: models.Wire("databricks-serving", id), Responses: r})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	checks, err := serving.Probe(ctx, p, targets,
		serving.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: 30 * time.Second})
	if err != nil {
		return errors.New("serving verification did not complete; no discovery/menu writes")
	}
	snapshot.Checks = serving.SanitizeChecks(checks)
	snapshot.CheckedAt = time.Now().UTC()
	return nil
}

// DefaultMenuLabel is the label an added id gets when none is given.
func DefaultMenuLabel(id string) string {
	var words []string
	prevDigits := false
	for w := range strings.SplitSeq(id, "-") {
		digits := w != "" && strings.Trim(w, "0123456789") == ""
		switch {
		case digits && prevDigits: // version runs read as one number: 5-5 → 5.5
			words[len(words)-1] += "." + w
		case menuAcronyms[w]:
			words = append(words, strings.ToUpper(w))
		case w != "":
			words = append(words, strings.ToUpper(w[:1])+w[1:])
		}
		prevDigits = digits
	}
	return strings.Join(words, " ")
}

// menuAcronyms are id words a label spells in capitals.
var menuAcronyms = map[string]bool{"glm": true, "gpt": true, "oss": true, "bge": true, "gte": true}

// MenuAdd returns menu with id added, or updated in place when present; catalog Responses-only ids default to Responses.
// responses is nil when the caller did not say. A blank label keeps the existing one, else the derived default.
func MenuAdd(menu []providers.OpencodeModel, id, label string, responses *bool) []providers.OpencodeModel {
	id = catalog.ID(id)
	out := slices.Clone(menu)
	i := slices.IndexFunc(out, func(m providers.OpencodeModel) bool { return m.ID == id })
	cur := providers.OpencodeModel{ID: id, Responses: models.ResponsesOnly(id)}
	if i >= 0 {
		cur = out[i]
	}
	if label != "" {
		cur.Label = label
	} else if cur.Label == "" {
		cur.Label = DefaultMenuLabel(id)
	}
	if responses != nil {
		cur.Responses = *responses
	}
	if i >= 0 {
		out[i] = cur
		return out
	}
	return append(out, cur)
}

// MenuRemove returns menu without id; ok is false when it was not there.
func MenuRemove(menu []providers.OpencodeModel, id string) (out []providers.OpencodeModel, ok bool) {
	id = catalog.ID(id)
	out = slices.DeleteFunc(slices.Clone(menu), func(m providers.OpencodeModel) bool { return m.ID == id })
	return out, len(out) != len(menu)
}

// ParseMenuImport reads an opencode.json (its databricks provider "models" maps) or plain `id[,Label][,responses]` lines.
func ParseMenuImport(data []byte) ([]providers.OpencodeModel, error) {
	if t := strings.TrimSpace(string(data)); strings.HasPrefix(t, "{") {
		return parseOpencodeJSON([]byte(t))
	}
	var out []providers.OpencodeModel
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m, err := parseMenuLine(line, n)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return dedupeMenu(out), sc.Err()
}

func parseMenuLine(line string, n int) (providers.OpencodeModel, error) {
	f := strings.Split(line, ",")
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	id := catalog.ID(f[0])
	if id == "" {
		return providers.OpencodeModel{}, fmt.Errorf("line %d: empty id", n)
	}
	m := providers.OpencodeModel{ID: id, Responses: models.ResponsesOnly(id)}
	if len(f) > 1 && f[1] != "" && f[1] != "responses" {
		m.Label = f[1]
	}
	if slices.Contains(f[1:], "responses") {
		m.Responses = true
	}
	if m.Label == "" {
		m.Label = DefaultMenuLabel(id)
	}
	return m, nil
}

// parseOpencodeJSON takes every databricks provider (by id or profile name); the provider id sets each model's routing.
func parseOpencodeJSON(data []byte) ([]providers.OpencodeModel, error) {
	var doc struct {
		Provider map[string]struct {
			NPM    string `json:"npm"`
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("opencode.json: %w", err)
	}
	var out []providers.OpencodeModel
	profiles := databricksProfileNames()
	for _, pid := range slices.Sorted(maps.Keys(doc.Provider)) {
		if !strings.Contains(pid, "databricks") && !namesProfile(pid, profiles) {
			continue
		}
		prov := doc.Provider[pid]
		for _, wire := range slices.Sorted(maps.Keys(prov.Models)) {
			id := catalog.ID(wire)
			label := strings.TrimSpace(strings.SplitN(prov.Models[wire].Name, " · ", 2)[0])
			if label == "" {
				label = DefaultMenuLabel(id)
			}
			out = append(out, providers.OpencodeModel{ID: id, Label: label, Responses: providerResponses(pid, prov.NPM)})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("opencode.json: no databricks provider with a models map")
	}
	return dedupeMenu(out), nil
}

// providerResponses reads the route from the provider's npm package (@ai-sdk/openai is Responses), else the "-gpt" suffix.
func providerResponses(pid, npm string) bool {
	if npm != "" {
		return npm == "@ai-sdk/openai"
	}
	return strings.HasSuffix(pid, "-gpt")
}

// databricksProfileNames are the registry's databricks profiles; a registry that does not load has none.
func databricksProfileNames() []string {
	reg, _ := providers.Load()
	if reg == nil {
		return nil
	}
	return providers.DatabricksProfiles(reg)
}

// namesProfile reports whether a profile appears in pid as whole "-"-delimited words (profiles may contain hyphens).
func namesProfile(pid string, profiles []string) bool {
	seg := "-" + pid + "-"
	return slices.ContainsFunc(profiles, func(p string) bool { return strings.Contains(seg, "-"+p+"-") })
}

// dedupeMenu keeps the last entry per id, in first-seen order.
func dedupeMenu(in []providers.OpencodeModel) []providers.OpencodeModel {
	var out []providers.OpencodeModel
	for _, m := range in {
		if i := slices.IndexFunc(out, func(o providers.OpencodeModel) bool { return o.ID == m.ID }); i >= 0 {
			out[i] = m
			continue
		}
		out = append(out, m)
	}
	return out
}

// MenuChange is one line of an import diff: Op is "+", "-" or "~".
type MenuChange struct {
	Op    string                   `json:"op"`
	Model providers.OpencodeModel  `json:"model"`
	Was   *providers.OpencodeModel `json:"was,omitempty"`
}

// DiffMenu lists what turns old into next: additions, removals, then label/responses changes.
func DiffMenu(old, next []providers.OpencodeModel) []MenuChange {
	var out []MenuChange
	find := func(l []providers.OpencodeModel, id string) (providers.OpencodeModel, bool) {
		i := slices.IndexFunc(l, func(m providers.OpencodeModel) bool { return m.ID == id })
		if i < 0 {
			return providers.OpencodeModel{}, false
		}
		return l[i], true
	}
	for _, m := range next {
		if _, ok := find(old, m.ID); !ok {
			out = append(out, MenuChange{Op: "+", Model: m})
		}
	}
	for _, m := range old {
		if _, ok := find(next, m.ID); !ok {
			out = append(out, MenuChange{Op: "-", Model: m})
		}
	}
	for _, m := range next {
		if was, ok := find(old, m.ID); ok && was != m {
			w := was
			out = append(out, MenuChange{Op: "~", Model: m, Was: &w})
		}
	}
	return out
}
