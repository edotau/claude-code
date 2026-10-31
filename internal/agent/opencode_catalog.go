package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeProvider resolves the workspace profile: --profile, else the OPENCODE_PROFILE pin.
func opencodeProvider(name string) (*providers.Registry, *providers.Provider, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, nil, err
	}
	if name == "" {
		if name = GetAgentRecord("opencode").Profile; name == "" {
			return nil, nil, errors.New("no profile pinned for opencode; pass --profile")
		}
	}
	p, err := reg.Get(name)
	return reg, p, err
}

// OpencodeServed lists the workspace's endpoints by name.
func OpencodeServed(name string) (map[string]providers.ModelInfo, *providers.Provider, error) {
	reg, p, err := opencodeProvider(name)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return nil, p, err
	}
	out := make(map[string]providers.ModelInfo, len(infos))
	for _, m := range infos {
		out[m.ID] = m
	}
	return out, p, nil
}

func ServedInfo(served map[string]providers.ModelInfo, p *providers.Provider, id string) (providers.ModelInfo, bool) {
	if m, ok := served[p.ModelPrefix+id]; ok {
		return m, true
	}
	m, ok := served[id]
	return m, ok
}

// OpencodeCheckTargets resolves the probe set: explicit ids, every served chat endpoint, or the menu.
func OpencodeCheckTargets(ids []string, all bool, served map[string]providers.ModelInfo, p *providers.Provider) []CheckTarget {
	var out []CheckTarget
	if all {
		for name, info := range served {
			if info.Task != "llm/v1/chat" {
				continue
			}
			id := MenuID(name)
			out = append(out, CheckTarget{ID: id, Wire: name, Responses: providers.ResponsesID(id)})
		}
		return out
	}
	if len(ids) == 0 {
		for _, m := range providers.OpencodeMenu() {
			ids = append(ids, m.ID)
		}
	}
	for _, raw := range ids {
		id := MenuID(raw)
		t := CheckTarget{ID: id, Responses: providers.ResponsesID(id)}
		if _, ok := served[p.ModelPrefix+id]; ok {
			t.Wire = p.ModelPrefix + id
		} else if _, ok := served[id]; ok {
			t.Wire = id
		} else {
			t.Missing = true
		}
		out = append(out, t)
	}
	return out
}

// OpencodeSyncModels resolves the profile; discovery validates identity before reading the sealed cache.
func OpencodeSyncModels(profile string, refresh bool) (*providers.Provider, ServingDiscovery, error) {
	_, p, err := opencodeProvider(profile)
	if err != nil {
		return nil, ServingDiscovery{}, errors.New("cannot resolve profile; check the provider configuration")
	}
	snapshot, err := DiscoverServingModels(context.Background(), p, refresh)
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
		id := MenuID(info.ID)
		if id != "" && models.Wire("databricks-serving", id) == info.ID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func OpencodeSyncMerge(menu []providers.OpencodeModel, ids []string, checks []CheckResult) ([]providers.OpencodeModel, int) {
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

func OpencodeSyncVerify(p *providers.Provider, snapshot *ServingDiscovery, ids []string, menu []providers.OpencodeModel) error {
	responses := make(map[string]bool, len(menu))
	for _, m := range menu {
		responses[m.ID] = m.Responses
	}
	targets := make([]CheckTarget, 0, len(ids))
	for _, id := range ids {
		r, exists := responses[id]
		if !exists {
			r = models.ResponsesOnly(id)
		}
		targets = append(targets, CheckTarget{ID: id, Wire: models.Wire("databricks-serving", id), Responses: r})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	checks, err := ProbeEndpoints(ctx, p, targets,
		CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: 30 * time.Second})
	if err != nil {
		return errors.New("serving verification did not complete; no discovery/menu writes")
	}
	snapshot.Checks = SanitizeServingChecks(checks)
	snapshot.CheckedAt = time.Now().UTC()
	return nil
}

// MenuID canonicalizes a typed model id to a menu id: a catalog id (any wire spelling) via models.Resolve, else
// the id with any catalog spelling prefix removed.
func MenuID(id string) string {
	id = strings.TrimSpace(models.Strip1M(id))
	if canonical, _, ok := models.Resolve(id); ok {
		return canonical
	}
	return stripSpellingPrefix(id)
}

// stripSpellingPrefix removes the id prefix a catalog spelling rule adds (derived from models.Wire, not listed here).
func stripSpellingPrefix(id string) string {
	for _, rule := range []string{"databricks-serving", "databricks-ai-gateway"} {
		if prefix := strings.TrimSuffix(models.Wire(rule, "x"), "x"); prefix != "" {
			if rest, ok := strings.CutPrefix(id, prefix); ok {
				return rest
			}
		}
	}
	return id
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
	id = MenuID(id)
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
	id = MenuID(id)
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
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		id := MenuID(f[0])
		if id == "" {
			return nil, fmt.Errorf("line %d: empty id", n)
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
		out = append(out, m)
	}
	return dedupeMenu(out), sc.Err()
}

// parseOpencodeJSON takes every provider whose id contains "databricks" (or has a models map when none does).
func parseOpencodeJSON(data []byte) ([]providers.OpencodeModel, error) {
	var doc struct {
		Provider map[string]struct {
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
	for _, pid := range slices.Sorted(mapKeys(doc.Provider)) {
		if !strings.Contains(pid, "databricks") && !namesProfile(pid, profiles) {
			continue
		}
		prov := doc.Provider[pid]
		for _, wire := range slices.Sorted(mapKeys(prov.Models)) {
			id := MenuID(wire)
			label := strings.TrimSpace(strings.SplitN(prov.Models[wire].Name, " · ", 2)[0])
			if label == "" {
				label = DefaultMenuLabel(id)
			}
			out = append(out, providers.OpencodeModel{ID: id, Label: label, Responses: models.ResponsesOnly(id)})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("opencode.json: no databricks provider with a models map")
	}
	return dedupeMenu(out), nil
}

// databricksProfileNames are the registry's databricks profiles; a registry that does not load has none.
func databricksProfileNames() []string {
	reg, _ := providers.Load()
	if reg == nil {
		return nil
	}
	return providers.DatabricksProfiles(reg)
}

// namesProfile reports whether a "-"-separated token of the opencode provider id is one of profiles.
func namesProfile(pid string, profiles []string) bool {
	return slices.ContainsFunc(strings.Split(pid, "-"), func(tok string) bool { return slices.Contains(profiles, tok) })
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
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
