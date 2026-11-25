package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	datedIn     = "# Decisions\n\n> c\n\n## 2026-11-25 — a <!-- id:0a1b2c3d -->\n\nbody a\n\n## 2026-11-24 — b <!-- id:1a1b2c3d -->\n\nbody b\n"
	bulletsIn   = "# Conv\n\n> c\n\n- one <!-- id:0a1b2c3d -->\n  wrapped\n\n- two <!-- id:1a1b2c3d -->\n"
	sectionsIn  = "# Active\n\n## Current Focus\n- a\n- b\n## Next Steps\n- c\n"
	repeatIn    = "# T\n\n## Now\n- a\n## Later\n- c\n## Now\n- z\n"
	noteIn      = "# T\n\n- a\n"
	tableBudget = 100 * time.Millisecond
	pointerB    = "Replaced by a0000001 (2026-11-26).\n"
)

func testEnv() Env {
	n := 0
	return Env{
		Now:   time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC),
		NewID: func(int, string) string { n++; return fmt.Sprintf("%08x", 0xa0000000+n) },
	}
}

// subst replaces "@N" in ID, Read and By with the contentRef of entry N as parsed.
func subst(d Doc, ops []Op) []Op {
	ref := func(s string) string {
		var n int
		if _, err := fmt.Sscanf(s, "@%d", &n); err != nil {
			return s
		}
		return contentRef(d.Entries[n].block())
	}
	out := slices.Clone(ops)
	for i := range out {
		out[i].ID, out[i].Read, out[i].By = ref(out[i].ID), ref(out[i].Read), ref(out[i].By)
	}
	return out
}

func status(rs []OpResult) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Outcome+":"+r.Reason)
	}
	return out
}

type applyCase struct {
	name, file, in, want string
	ops                  []Op
	status               []string
}

func applyCases() []applyCase {
	ok, add := []string{"ok:"}, Op{Op: "add", Title: "n", Text: "t"}
	topN := "# Decisions\n\n> c\n\n## 2026-11-26 — n <!-- id:a0000001 -->\n\nt\n\n"
	rest := "## 2026-11-25 — a <!-- id:0a1b2c3d -->\n\nbody a\n\n## 2026-11-24 — b <!-- id:1a1b2c3d -->\n\nbody b\n"
	markedB := "## 2026-11-24 — ~~b~~ <!-- id:1a1b2c3d -->\nReplaced by 0a1b2c3d (2026-11-26).\n\nbody b\n"
	return []applyCase{
		{"dated add", "decisionLog.md", datedIn, topN + rest, []Op{add}, ok},
		{"dated update body", "decisionLog.md", datedIn, strings.Replace(datedIn, "body a", "new body", 1),
			[]Op{{Op: "update", ID: "0a1b2c3d", Read: "@0", Text: "new body"}}, ok},
		{"dated update title", "decisionLog.md", datedIn, strings.Replace(datedIn, "— a <", "— A2 <", 1),
			[]Op{{Op: "update", ID: "0a1b2c3d", Read: "@0", Title: "A2"}}, ok},
		{"dated supersede writes both sides", "decisionLog.md", datedIn,
			"# Decisions\n\n> c\n\n## 2026-11-26 — c <!-- id:a0000001 -->\nSupersedes 1a1b2c3d.\n\nt\n\n" +
				"## 2026-11-25 — a <!-- id:0a1b2c3d -->\n\nbody a\n\n## 2026-11-24 — ~~b~~ <!-- id:1a1b2c3d -->\n" + pointerB + "\nbody b\n",
			[]Op{{Op: "supersede", ID: "1a1b2c3d", Read: "@1", Title: "c", Text: "t"}}, ok},
		{"dated close superseded-by", "decisionLog.md", datedIn, strings.Replace(datedIn, "## 2026-11-24 — b <!-- id:1a1b2c3d -->\n", markedB[:len(markedB)-len("\nbody b\n")], 1),
			[]Op{{Op: "close", ID: "1a1b2c3d", Read: "@1", Reason: "superseded-by", By: "0a1b2c3d"}}, ok},
		{"dated close done is append-only", "decisionLog.md", datedIn, datedIn,
			[]Op{{Op: "close", ID: "1a1b2c3d", Read: "@1", Reason: "done"}},
			[]string{"failed:append-only: a dated entry closes only as superseded-by"}},
		{"close by a dead id", "decisionLog.md", datedIn, datedIn,
			[]Op{{Op: "close", ID: "1a1b2c3d", Read: "@1", Reason: "superseded-by", By: "ffffffff"}},
			[]string{"failed:by is not a live id"}},
		{"duplicate add is a noop", "decisionLog.md", datedIn, datedIn,
			[]Op{{Op: "add", Title: "a", Text: "body a"}}, []string{"noop:duplicate"}},
		{"stale target fails alone", "decisionLog.md", datedIn, topN + rest,
			[]Op{{Op: "update", ID: "0a1b2c3d", Read: "z00000000", Text: "x"}, add},
			[]string{"failed:changed since read", "ok:"}},
		{"unknown id", "decisionLog.md", datedIn, datedIn,
			[]Op{{Op: "update", ID: "ffffffff", Read: "z00000000", Text: "x"}}, []string{"failed:unknown id"}},
		{"noop op", "decisionLog.md", datedIn, datedIn, []Op{{Op: "noop", Note: "x"}}, []string{"noop:no change"}},
		{"batch order, newest on top", "decisionLog.md", datedIn,
			"# Decisions\n\n> c\n\n## 2026-11-26 — n2 <!-- id:a0000002 -->\n\nt\n\n## 2026-11-26 — n1 <!-- id:a0000001 -->\n\nt\n\n" + rest,
			[]Op{{Op: "add", Title: "n1", Text: "t"}, {Op: "add", Title: "n2", Text: "t"}}, []string{"ok:", "ok:"}},
		{"close by an id added earlier in the batch", "decisionLog.md", datedIn,
			topN + strings.Replace(rest, "## 2026-11-24 — b <!-- id:1a1b2c3d -->\n", "## 2026-11-24 — ~~b~~ <!-- id:1a1b2c3d -->\n"+pointerB, 1),
			[]Op{add, {Op: "close", ID: "1a1b2c3d", Read: "@1", Reason: "superseded-by", By: "a0000001"}}, []string{"ok:", "ok:"}},
	}
}

func bulletCases() []applyCase {
	two := "- two <!-- id:1a1b2c3d -->\n"
	one := "- one <!-- id:0a1b2c3d -->\n  wrapped\n"
	head := "# Conv\n\n> c\n\n"
	ok := []string{"ok:"}
	return []applyCase{
		{"bullets add at the bottom", "conventions.md", bulletsIn, head + one + "\n" + two + "\n- three <!-- id:a0000001 -->\n  more\n",
			[]Op{{Op: "add", Text: "three\nmore"}}, ok},
		{"wrapped bullet updates as one unit", "conventions.md", bulletsIn, head + "- uno <!-- id:0a1b2c3d -->\n  continued\n\n" + two,
			[]Op{{Op: "update", ID: "0a1b2c3d", Read: "@0", Text: "uno\ncontinued"}}, ok},
		{"bullets supersede writes both sides", "conventions.md", bulletsIn,
			head + one + "\n- ~~two~~ <!-- id:1a1b2c3d -->\n  " + pointerB + "\n- deux <!-- id:a0000001 -->\n  Supersedes 1a1b2c3d.\n",
			[]Op{{Op: "supersede", ID: "1a1b2c3d", Read: "@1", Text: "deux"}}, ok},
		{"bullets close done", "conventions.md", bulletsIn, head + one,
			[]Op{{Op: "close", ID: "1a1b2c3d", Read: "@1", Reason: "done"}}, ok},
		{"bullets close stale", "conventions.md", bulletsIn, head + two,
			[]Op{{Op: "close", ID: "0a1b2c3d", Read: "@0", Reason: "stale"}}, ok},
		{"bullets close superseded-by", "conventions.md", bulletsIn, head + two,
			[]Op{{Op: "close", ID: "0a1b2c3d", Read: "@0", Reason: "superseded-by", By: "1a1b2c3d"}}, ok},
	}
}

func sectionCases() []applyCase {
	ok := []string{"ok:"}
	return []applyCase{
		{"section add on top", "activeContext.md", sectionsIn, "# Active\n\n## Current Focus\n- new\n- a\n- b\n## Next Steps\n- c\n",
			[]Op{{Op: "add", Target: "activeContext", Section: "Current Focus", Text: "new"}}, ok},
		{"section created in canonical order", "activeContext.md", sectionsIn,
			"# Active\n\n## Current Focus\n- a\n- b\n## Blockers\n- x\n## Next Steps\n- c\n",
			[]Op{{Op: "add", Target: "activeContext", Section: "Blockers", Text: "x"}}, ok},
		{"section created last", "activeContext.md", sectionsIn, sectionsIn + "## Parked\n- p\n",
			[]Op{{Op: "add", Target: "activeContext", Section: "Parked", Text: "p"}}, ok},
		{"unknown section fails", "activeContext.md", sectionsIn, sectionsIn,
			[]Op{{Op: "add", Target: "activeContext", Section: "Nope", Text: "x"}}, []string{"failed:unknown section \"Nope\" for activeContext"}},
		{"repeated heading adds to the first", "progress.md", repeatIn, "# T\n\n## Now\n- new\n- a\n## Later\n- c\n## Now\n- z\n",
			[]Op{{Op: "add", Section: "Now", Text: "new"}}, ok},
		{"state bullet updates by content ref", "activeContext.md", sectionsIn, strings.Replace(sectionsIn, "- b", "- bb", 1),
			[]Op{{Op: "update", ID: "@2", Text: "bb"}}, ok},
		{"state bullet closes by content ref", "activeContext.md", sectionsIn, strings.Replace(sectionsIn, "- a\n", "", 1),
			[]Op{{Op: "close", ID: "@1", Reason: "done"}}, ok},
		{"a section heading is not an entry", "activeContext.md", sectionsIn, sectionsIn,
			[]Op{{Op: "close", ID: "@0", Reason: "stale"}}, []string{"failed:not an entry"}},
		{"note bullet goes to the bottom", "note.md", noteIn, "# T\n\n- a\n- b\n", []Op{{Op: "add", Text: "b"}}, ok},
	}
}

func TestApplyOps(t *testing.T) {
	start := time.Now()
	var cases []applyCase
	cases = append(append(append(cases, applyCases()...), bulletCases()...), sectionCases()...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := ParseDoc(c.file, []byte(c.in))
			out, res := Apply(in, Batch{Ops: subst(in, c.ops)}, testEnv())
			if got := string(out.Render()); got != c.want {
				t.Errorf("render\n got %q\nwant %q", got, c.want)
			}
			if got := status(res); !slices.Equal(got, c.status) {
				t.Errorf("status %q, want %q", got, c.status)
			}
			if !reflect.DeepEqual(ParseDoc(c.file, out.Render()), out) {
				t.Errorf("result does not re-parse to itself")
			}
			keepsUntouched(t, in, out, res)
		})
	}
	if d := time.Since(start); d > tableBudget {
		t.Errorf("table took %v, want under 100ms", d)
	}
}

// keepsUntouched requires every entry no result names to keep its bytes (the last may gain a separator).
func keepsUntouched(t *testing.T, in, out Doc, res []OpResult) {
	t.Helper()
	named := map[string]bool{}
	for _, r := range res {
		named[r.ID], named[r.Op.ID] = true, true
	}
	for i, e := range in.Entries {
		if named[e.refOf()] || i == len(in.Entries)-1 {
			continue
		}
		if !slices.ContainsFunc(out.Entries, func(o DocEntry) bool { return slices.Equal(o.Lines, e.Lines) }) {
			t.Errorf("untouched entry %d changed: %q", i, e.Lines)
		}
	}
}

func TestApplyKeepsInput(t *testing.T) {
	for _, c := range applyCases() {
		in := ParseDoc(c.file, []byte(c.in))
		want := ParseDoc(c.file, []byte(c.in))
		Apply(in, Batch{Ops: subst(in, c.ops)}, testEnv())
		if !reflect.DeepEqual(in, want) {
			t.Errorf("%s: input Doc changed", c.name)
		}
	}
}

func TestApplyEmptyAndNoopKeepBytes(t *testing.T) {
	for p, data := range storeFixtures(t) {
		d := ParseDoc(p, data)
		for _, b := range []Batch{{}, {Ops: []Op{{Op: "noop", Note: "n"}}}} {
			out, _ := Apply(d, b, testEnv())
			if !bytes.Equal(out.Render(), data) {
				t.Errorf("%s: bytes changed by %d-op batch", p, len(b.Ops))
			}
		}
	}
}

func TestApplyRejectsBadOps(t *testing.T) {
	rows := []struct {
		name string
		op   Op
		want string
	}{
		{"unknown op", Op{Op: "delete"}, "unknown op"},
		{"unknown reason", Op{Op: "close", ID: "z0a1b2c3d", Reason: "gone"}, "unknown close reason"},
		{"superseded-by without by", Op{Op: "close", ID: "z0a1b2c3d", Reason: "superseded-by"}, "by is required"},
		{"by with another reason", Op{Op: "close", ID: "z0a1b2c3d", Reason: "done", By: "0a1b2c3d"}, "by is required"},
		{"close without reason", Op{Op: "close", ID: "z0a1b2c3d"}, "close needs a reason"},
		{"stored id without read", Op{Op: "update", ID: "0a1b2c3d", Text: "x"}, "needs read"},
		{"id marker in text", Op{Op: "add", Text: "x <!-- id:0a1b2c3d -->"}, "id marker"},
		{"two-line title", Op{Op: "add", Title: "a\nb"}, "one line"},
		{"text over the cap", Op{Op: "add", Text: strings.Repeat("x", maxOpText+1)}, "over"},
		{"unknown target", Op{Op: "add", Target: "x", Text: "t"}, "unknown target"},
		{"add without content", Op{Op: "add"}, "needs a title or text"},
		{"bad ref", Op{Op: "update", ID: "xyz", Text: "t"}, "not a ref"},
	}
	for _, r := range rows {
		_, res := Apply(ParseDoc("decisionLog.md", []byte(datedIn)), Batch{Ops: []Op{r.op}}, testEnv())
		if res[0].Outcome != DreamFailed || !strings.Contains(res[0].Reason, r.want) {
			t.Errorf("%s: got %+v, want failure containing %q", r.name, res[0], r.want)
		}
		if err := (Batch{Ops: []Op{r.op}}).Validate(); err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("%s: Validate = %v", r.name, err)
		}
	}
	if err := (Batch{Ops: make([]Op, maxBatchOps+1)}).Validate(); err == nil {
		t.Error("61 ops passed Validate")
	}
	if (Batch{Ops: []Op{}}).Validate() != nil || (Batch{}).Validate() == nil {
		t.Error("empty ops must pass and absent ops must fail")
	}
}

func TestApplyRejectsGrammarBreakers(t *testing.T) {
	dated := ParseDoc("decisionLog.md", []byte(datedIn))
	for _, text := range []string{"ok\n## 2026-01-01 — sneaky", "```\nopen fence"} {
		_, res := Apply(dated, Batch{Ops: []Op{{Op: "add", Title: "n", Text: text}}}, testEnv())
		if res[0].Outcome != DreamFailed {
			t.Errorf("text %q was accepted: %+v", text, res[0])
		}
	}
	bul := ParseDoc("conventions.md", []byte(bulletsIn))
	out, res := Apply(bul, Batch{Ops: []Op{{Op: "add", Text: "a\n- b\n## c"}}}, testEnv())
	if res[0].Outcome != DreamOK || len(out.Entries) != len(bul.Entries)+1 {
		t.Errorf("indented continuation lines must stay one entry: %+v", res[0])
	}
}

func TestBatchSchemaMatchesOp(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal([]byte(BatchSchema()), &s); err != nil {
		t.Fatal(err)
	}
	item := s["properties"].(map[string]any)["ops"].(map[string]any)["items"].(map[string]any)
	props := item["properties"].(map[string]any)
	var got, rules []string
	for k := range props {
		got = append(got, k)
	}
	for k := range fieldRules {
		rules = append(rules, k)
	}
	slices.Sort(got)
	slices.Sort(rules)
	tags := opTags()
	slices.Sort(tags)
	if !slices.Equal(got, tags) || !slices.Equal(rules, tags) {
		t.Fatalf("schema fields %v, rules %v, Op json fields %v: all three must match", got, rules, tags)
	}
	for field, want := range map[string][]string{"op": opNames, "reason": closeReasons, "target": targets} {
		enum, _ := json.Marshal(props[field].(map[string]any)["enum"])
		wantJSON, _ := json.Marshal(want)
		if string(enum) != string(wantJSON) {
			t.Errorf("%s enum %s, want %s", field, enum, wantJSON)
		}
	}
}

func TestBatchSchemaRejects(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(BatchSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	for _, r := range schemaRows() {
		var v any
		if err := json.Unmarshal([]byte(r.in), &v); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if got := schemaOK(schema, v); got != r.valid {
			t.Errorf("%s: schema says %v, want %v", r.name, got, r.valid)
		}
		var b Batch
		dec := json.NewDecoder(strings.NewReader(r.in))
		dec.DisallowUnknownFields()
		if goOK := dec.Decode(&b) == nil && b.Validate() == nil; goOK != r.valid {
			t.Errorf("%s: Go says %v, want %v", r.name, goOK, r.valid)
		}
	}
	var b Batch
	dec := json.NewDecoder(strings.NewReader(`{"ops":[{"op":"noop","x":1}]}`))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) == nil || schemaOK(schema, map[string]any{"ops": []any{map[string]any{"op": "noop", "x": 1.0}}}) {
		t.Error("an unknown field passed")
	}
}

type schemaRow struct {
	name, in string
	valid    bool
}

func schemaRows() []schemaRow {
	over := `{"ops":[` + strings.Repeat(`{"op":"noop"},`, maxBatchOps) + `{"op":"noop"}]}`
	return []schemaRow{
		{"add", `{"ops":[{"op":"add","title":"t","text":"x"}]}`, true},
		{"update by content ref", `{"ops":[{"op":"update","id":"z0a1b2c3d","text":"x"}]}`, true},
		{"update by stored id", `{"ops":[{"op":"update","id":"0a1b2c3d","read":"z0a1b2c3d","text":"x"}]}`, true},
		{"close done", `{"ops":[{"op":"close","id":"z0a1b2c3d","reason":"done"}]}`, true},
		{"close superseded-by", `{"ops":[{"op":"close","id":"z0a1b2c3d","reason":"superseded-by","by":"1a1b2c3d"}]}`, true},
		{"noop", `{"ops":[{"op":"noop","note":"n"}]}`, true},
		{"unknown op", `{"ops":[{"op":"delete"}]}`, false},
		{"unknown reason", `{"ops":[{"op":"close","id":"z0a1b2c3d","reason":"gone"}]}`, false},
		{"superseded-by without by", `{"ops":[{"op":"close","id":"z0a1b2c3d","reason":"superseded-by"}]}`, false},
		{"by with done", `{"ops":[{"op":"close","id":"z0a1b2c3d","reason":"done","by":"1a1b2c3d"}]}`, false},
		{"close without reason", `{"ops":[{"op":"close","id":"z0a1b2c3d"}]}`, false},
		{"stored id without read", `{"ops":[{"op":"update","id":"0a1b2c3d","text":"x"}]}`, false},
		{"bad id", `{"ops":[{"op":"update","id":"nope","text":"x"}]}`, false},
		{"add without content", `{"ops":[{"op":"add"}]}`, false},
		{"add with id", `{"ops":[{"op":"add","id":"z0a1b2c3d","text":"x"}]}`, false},
		{"reason on add", `{"ops":[{"op":"add","text":"x","reason":"done"}]}`, false},
		{"two-line title", `{"ops":[{"op":"add","title":"a\nb"}]}`, false},
		{"too many ops", over, false},
		{"missing ops", `{}`, false},
	}
}

// schemaOK checks v against the subset of JSON Schema BatchSchema uses.
func schemaOK(s map[string]any, v any) bool {
	for k, arg := range s {
		if !keywordOK(k, arg, s, v) {
			return false
		}
	}
	return true
}

func keywordOK(k string, arg any, s map[string]any, v any) bool {
	switch k {
	case "type":
		return typeOK(arg.(string), v)
	case "enum":
		return slices.ContainsFunc(toAny(arg), func(e any) bool { return e == v })
	case "const":
		return arg == v
	case "pattern", "maxLength":
		return stringOK(k, arg, v)
	}
	return structOK(k, arg, s, v)
}

func structOK(k string, arg any, s map[string]any, v any) bool {
	obj, _ := v.(map[string]any)
	switch k {
	case "required":
		return obj == nil || allKeys(toAny(arg), obj)
	case "properties":
		return propsOK(arg.(map[string]any), obj)
	case "additionalProperties":
		return arg != false || obj == nil || len(obj) == len(matching(s["properties"], obj))
	case "maxItems":
		a, isArr := v.([]any)
		return !isArr || float64(len(a)) <= toFloat(arg)
	case "items":
		a, _ := v.([]any)
		return !slices.ContainsFunc(a, func(e any) bool { return !schemaOK(arg.(map[string]any), e) })
	}
	return combinatorOK(k, arg, s, v)
}

func combinatorOK(k string, arg any, s map[string]any, v any) bool {
	switch k {
	case "allOf":
		return !slices.ContainsFunc(toSchemas(arg), func(c map[string]any) bool { return !schemaOK(c, v) })
	case "anyOf":
		return slices.ContainsFunc(toSchemas(arg), func(c map[string]any) bool { return schemaOK(c, v) })
	case "not":
		return !schemaOK(arg.(map[string]any), v)
	case "if":
		then, has := s["then"].(map[string]any)
		return !schemaOK(arg.(map[string]any), v) || !has || schemaOK(then, v)
	}
	return true // "then" runs under "if"; "$schema" is a label
}

func typeOK(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	}
	_, ok := v.(string)
	return ok
}

func stringOK(k string, arg, v any) bool {
	s, ok := v.(string)
	if !ok {
		return true
	}
	if k == "maxLength" {
		return float64(utf8.RuneCountInString(s)) <= toFloat(arg)
	}
	return regexp.MustCompile(arg.(string)).MatchString(s)
}

func allKeys(keys []any, obj map[string]any) bool {
	return !slices.ContainsFunc(keys, func(k any) bool { _, ok := obj[k.(string)]; return !ok })
}

func propsOK(props, obj map[string]any) bool {
	for name, sub := range props {
		if val, ok := obj[name]; ok && !schemaOK(sub.(map[string]any), val) {
			return false
		}
	}
	return true
}

func matching(props any, obj map[string]any) map[string]any {
	out := map[string]any{}
	p, _ := props.(map[string]any)
	for k, v := range obj {
		if _, ok := p[k]; ok {
			out[k] = v
		}
	}
	return out
}

func toAny(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	var out []any
	for _, s := range v.([]string) {
		out = append(out, s)
	}
	return out
}

func toSchemas(v any) []map[string]any {
	var out []map[string]any
	switch a := v.(type) {
	case []any:
		for _, e := range a {
			out = append(out, e.(map[string]any))
		}
	case []map[string]any:
		out = a
	}
	return out
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return 0
}
