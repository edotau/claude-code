package catalog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeSyncIDs selects unique, losslessly spellable READY chat endpoint ids in deterministic order.
func opencodeSyncIDs(infos []providers.ModelInfo) []string {
	var ids []string
	for _, info := range infos {
		if info.Gateway || !info.ReadyChat() ||
			!strings.HasPrefix(info.ID, models.DatabricksPrefix) {
			continue
		}
		id := ID(info.ID)
		if id != "" && models.Wire("databricks-serving", id) == info.ID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func opencodeSyncMerge(menu []providers.OpencodeModel, ids []string, checks []serving.CheckResult) ([]providers.OpencodeModel, int) {
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
			// menuAdd canonicalizes its input, so pass the lossless wire name rather than canonicalizing the id twice.
			next = menuAdd(next, models.Wire("databricks-serving", id), "", nil)
		}
	}
	return next, skipped
}

func opencodeSyncVerify(p *providers.Provider, snapshot *serving.Snapshot, ids []string, menu []providers.OpencodeModel) error {
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

// defaultMenuLabel is the label an added id gets when none is given.
func defaultMenuLabel(id string) string {
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

// menuAdd returns menu with id added, or updated in place when present; catalog Responses-only ids default to Responses.
// responses is nil when the caller did not say. A blank label keeps the existing one, else the derived default.
func menuAdd(menu []providers.OpencodeModel, id, label string, responses *bool) []providers.OpencodeModel {
	id = ID(id)
	out := slices.Clone(menu)
	i := slices.IndexFunc(out, func(m providers.OpencodeModel) bool { return m.ID == id })
	cur := providers.OpencodeModel{ID: id, Responses: models.ResponsesOnly(id)}
	if i >= 0 {
		cur = out[i]
	}
	if label != "" {
		cur.Label = label
	} else if cur.Label == "" {
		cur.Label = defaultMenuLabel(id)
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
	id = ID(id)
	out = slices.DeleteFunc(slices.Clone(menu), func(m providers.OpencodeModel) bool { return m.ID == id })
	return out, len(out) != len(menu)
}

// parseMenuImport reads an opencode.json (its databricks provider "models" maps) or plain `id[,Label][,responses]` lines.
func parseMenuImport(data []byte) ([]providers.OpencodeModel, error) {
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
	id := ID(f[0])
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
		m.Label = defaultMenuLabel(id)
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
			id := ID(wire)
			label := strings.TrimSpace(strings.SplitN(prov.Models[wire].Name, " · ", 2)[0])
			if label == "" {
				label = defaultMenuLabel(id)
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

// diffMenu lists what turns old into next: additions, removals, then label/responses changes.
func diffMenu(old, next []providers.OpencodeModel) []MenuChange {
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

// MenuRow is one `opencode catalog list` line; Live, Ready and Check are set only for a live listing.
type MenuRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses"`
	Source    string `json:"source"`
	Live      string `json:"live,omitempty"`
	Ready     string `json:"ready,omitempty"`
	Check     string `json:"check,omitempty"`
}

// MenuRows lists the shared menu; a non-nil p marks each entry served or missing among infos, its workspace listing.
func MenuRows(p *providers.Provider, infos []providers.ModelInfo) []MenuRow {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	live, served := p != nil, byName(infos)
	var cache map[string]serving.CheckResult
	if live {
		cache, _ = LoadCheckCache(p.Name)
	}
	var rows []MenuRow
	for _, m := range providers.OpencodeMenu() {
		r := MenuRow{ID: m.ID, Label: m.Label, Responses: m.Responses, Source: src}
		if live {
			r.Live = "missing"
			if info, ok := servedInfo(served, p, m.ID); ok {
				r.Live, r.Ready = "served", info.Ready
			}
			r.Check = checkCell(cache, m.ID)
		}
		rows = append(rows, r)
	}
	return rows
}

// MenuAddServed is the menu with arg (`id[=Label]`) added; unless force, lookup's workspace listing must serve the id.
func MenuAddServed(arg string, responses, responsesSet, force bool,
	lookup func() (*providers.Provider, []providers.ModelInfo, error)) (id string, next []providers.OpencodeModel, err error) {
	id, label, _ := strings.Cut(arg, "=")
	id = ID(id)
	if id == "" {
		return "", nil, errors.New("empty model id")
	}
	if !force {
		p, infos, err := lookup()
		if err != nil {
			return "", nil, fmt.Errorf("cannot check %s against the workspace (--force skips): %v", id, err)
		}
		if _, ok := servedInfo(byName(infos), p, id); !ok {
			return "", nil, fmt.Errorf("%s%s is not served by %s (--force skips the check)", p.ModelPrefix, id, p.Name)
		}
	}
	var r *bool
	if responsesSet {
		r = &responses
	}
	return id, menuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r), nil
}

// MenuImportDiff parses file and returns the menu it describes plus what changes against the current one.
func MenuImportDiff(file string) (next []providers.OpencodeModel, changes []MenuChange, err error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	next, err = parseMenuImport(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", file, err)
	}
	return next, diffMenu(providers.OpencodeMenu(), next), nil
}

// MenuCheck probes the chosen endpoints of infos and caches the results; cacheErr is a cache write failure only.
func MenuCheck(p *providers.Provider, infos []providers.ModelInfo, ids []string, all bool, timeout time.Duration,
) (results []serving.CheckResult, cacheErr, err error) {
	served := byName(infos)
	var menu []string
	for _, m := range providers.OpencodeMenu() {
		menu = append(menu, m.ID)
	}
	return check(p, targets(ids, all, served, p, menu), serving.CheckOptions{Concurrency: 4, Timeout: timeout})
}

// SyncOptions are `opencode catalog sync`'s switches.
type SyncOptions struct {
	Apply  bool
	Verify bool
}

// SyncPlan is a discovery run merged into the shared menu; nothing is written until Seal and the caller's menu write.
type SyncPlan struct {
	Provider *providers.Provider
	Snapshot serving.Snapshot
	IDs      []string // eligible serving ids
	Menu     []providers.OpencodeModel
	Next     []providers.OpencodeModel
	Skipped  int // additions dropped because their probe failed
	Changes  []MenuChange
	Verified bool
}

// MenuSync merges snapshot's serving models absent from the shared menu; existing order, labels and Responses stay.
func MenuSync(p *providers.Provider, snapshot serving.Snapshot, opts SyncOptions) (SyncPlan, error) {
	ids := opencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := opencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return SyncPlan{}, err
		}
	}
	next, skipped := opencodeSyncMerge(menu, ids, snapshot.Checks)
	return SyncPlan{Provider: p, Snapshot: snapshot, IDs: ids, Menu: menu, Next: next, Skipped: skipped,
		Changes: diffMenu(menu, next), Verified: opts.Verify}, nil
}

// Seal stores the discovery snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func (pl SyncPlan) Seal() (sealed bool, err error) {
	return Seal(pl.Provider, pl.Snapshot, pl.Verified)
}
