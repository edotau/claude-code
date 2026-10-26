package providers

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// MenuID canonicalizes a typed model id to a menu id: Claude ids via models.Canonical, then the databricks- prefix off.
func MenuID(id string) string {
	base, _ := models.Canonical(id)
	return strings.TrimPrefix(base, "databricks-")
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

// MenuAdd returns menu with id added, or updated in place when present; gpt- ids default to Responses.
// responses is nil when the caller did not say. A blank label keeps the existing one, else the derived default.
func MenuAdd(menu []OpencodeModel, id, label string, responses *bool) []OpencodeModel {
	id = MenuID(id)
	out := slices.Clone(menu)
	i := slices.IndexFunc(out, func(m OpencodeModel) bool { return m.ID == id })
	cur := OpencodeModel{ID: id, Responses: strings.HasPrefix(id, "gpt-")}
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
func MenuRemove(menu []OpencodeModel, id string) (out []OpencodeModel, ok bool) {
	id = MenuID(id)
	out = slices.DeleteFunc(slices.Clone(menu), func(m OpencodeModel) bool { return m.ID == id })
	return out, len(out) != len(menu)
}

// ParseMenuImport reads an opencode.json (its databricks provider "models" maps) or plain `id[,Label][,responses]` lines.
func ParseMenuImport(data []byte) ([]OpencodeModel, error) {
	if t := strings.TrimSpace(string(data)); strings.HasPrefix(t, "{") {
		return parseOpencodeJSON([]byte(t))
	}
	var out []OpencodeModel
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
		m := OpencodeModel{ID: id, Responses: strings.HasPrefix(id, "gpt-")}
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
func parseOpencodeJSON(data []byte) ([]OpencodeModel, error) {
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
	var out []OpencodeModel
	for _, pid := range slices.Sorted(mapKeys(doc.Provider)) {
		if !strings.Contains(pid, "databricks") && !isDatabricksEnvID(pid) {
			continue
		}
		prov := doc.Provider[pid]
		for _, wire := range slices.Sorted(mapKeys(prov.Models)) {
			id := MenuID(wire)
			label := strings.TrimSpace(strings.SplitN(prov.Models[wire].Name, " · ", 2)[0])
			if label == "" {
				label = DefaultMenuLabel(id)
			}
			out = append(out, OpencodeModel{ID: id, Label: label,
				Responses: strings.HasSuffix(pid, "-gpt")})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("opencode.json: no databricks provider with a models map")
	}
	return dedupeMenu(out), nil
}

func isDatabricksEnvID(id string) bool {
	id = strings.TrimPrefix(strings.TrimSuffix(id, "-gpt"), "harness-") // the render names rows harness-<env>[-gpt]
	return slices.Contains(DatabricksEnvs, id)
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
func dedupeMenu(in []OpencodeModel) []OpencodeModel {
	var out []OpencodeModel
	for _, m := range in {
		if i := slices.IndexFunc(out, func(o OpencodeModel) bool { return o.ID == m.ID }); i >= 0 {
			out[i] = m
			continue
		}
		out = append(out, m)
	}
	return out
}

// MenuChange is one line of an import diff: Op is "+", "-" or "~".
type MenuChange struct {
	Op    string         `json:"op"`
	Model OpencodeModel  `json:"model"`
	Was   *OpencodeModel `json:"was,omitempty"`
}

// DiffMenu lists what turns old into next: additions, removals, then label/responses changes.
func DiffMenu(old, next []OpencodeModel) []MenuChange {
	var out []MenuChange
	find := func(l []OpencodeModel, id string) (OpencodeModel, bool) {
		i := slices.IndexFunc(l, func(m OpencodeModel) bool { return m.ID == id })
		if i < 0 {
			return OpencodeModel{}, false
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
