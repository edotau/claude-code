package wire

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStructuredRejectsMalformedConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, schema, reason string
	}{
		{"required string", `{"type":"object","required":"approved"}`, "required must be an array"},
		{"required non-string name", `{"required":[1]}`, "required must hold only property names"},
		{"properties array", `{"properties":[]}`, "properties must be an object"},
		{"enum object", `{"enum":{}}`, "enum must be an array"},
		{"absent property", `{"properties":{"absent":{"required":"approved"}}}`, "required must be an array"},
		{"unused items", `{"items":{"properties":[]}}`, "properties must be an object"},
		{"unused tuple item", `{"items":[true,{"required":[false]}]}`, "required must hold only property names"},
		{"unused additional items", `{"additionalItems":{"enum":false}}`, "enum must be an array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, unchecked, err := Structured(`{}`, tc.schema)
			if !errors.Is(err, errSchema) {
				t.Fatalf("malformed schema: got raw=%s, unchecked=%v, err=%v; want errSchema", raw, unchecked, err)
			}
			if !strings.Contains(err.Error(), "malformed schema") || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v, want malformed schema identifying %q", err, tc.reason)
			}
			if raw != nil || unchecked != nil {
				t.Errorf("malformed schema returned raw=%s, unchecked=%v; want neither", raw, unchecked)
			}
		})
	}
}

func TestStructuredRequiredEmptyPropertyName(t *testing.T) {
	const schema = `{"type":"object","required":[""]}`
	if _, _, err := Structured(`{}`, schema); !errors.Is(err, errSchema) || !strings.Contains(err.Error(), `missing required key ""`) {
		t.Fatalf("missing empty-string key: got %v, want errSchema identifying the key", err)
	}
	const answer = `{"":null}`
	raw, unchecked, err := Structured(answer, schema)
	if err != nil {
		t.Fatalf("present empty-string key rejected: %v", err)
	}
	if string(raw) != answer || len(unchecked) != 0 {
		t.Errorf("raw = %s, unchecked = %v; want %s, none", raw, unchecked, answer)
	}
}

func TestStructuredTupleItems(t *testing.T) {
	for _, tc := range []struct {
		name, answer, schema string
		wantErr              bool
		unchecked            []string
	}{
		{"wrong first item", `["wrong"]`, `{"type":"array","items":[{"type":"integer"}]}`, true, nil},
		{"wrong second item", `[1,2]`, `{"items":[{"type":"integer"},{"type":"string"}]}`, true, nil},
		{"matching positions", `[1,"ok"]`, `{"items":[{"type":"integer"},{"type":"string"}]}`, false, nil},
		{"shorter tuple", `[1]`, `{"items":[{"type":"integer"},{"type":"string"}]}`, false, nil},
		{"extra allowed by default", `[1,"extra"]`, `{"items":[{"type":"integer"}]}`, false, nil},
		{"extra forbidden", `[1,"extra"]`, `{"items":[{"type":"integer"}],"additionalItems":false}`, true, nil},
		{"wrong extra type", `[1,2]`, `{"items":[{"type":"integer"}],"additionalItems":{"type":"string"}}`, true, nil},
		{"matching extra type", `[1,"ok"]`, `{"items":[{"type":"integer"}],"additionalItems":{"type":"string"}}`, false, nil},
		{"unchecked nested keywords", `[1,"ok"]`, `{"items":[{"minimum":0}],"additionalItems":{"pattern":"ok"}}`, false, []string{"minimum", "pattern"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, unchecked, err := Structured(tc.answer, tc.schema)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Structured(%s, %s): err = %v, want error = %v", tc.answer, tc.schema, err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, errSchema) {
					t.Errorf("err = %v, want errSchema", err)
				}
				return
			}
			if string(raw) != tc.answer || !slices.Equal(unchecked, tc.unchecked) {
				t.Errorf("raw = %s, unchecked = %v; want %s, %v", raw, unchecked, tc.answer, tc.unchecked)
			}
		})
	}
}

func TestStructuredAnswerNested(t *testing.T) {
	const schema = `{"type":"object","required":["a"],"properties":{"a":{"type":"object","required":["b"],` +
		`"properties":{"b":{"type":"string","enum":["x","y"]},"l":{"type":"array","items":{"type":"integer"}}}},"c":{"minimum":1}}}`
	for answer, wantErr := range map[string]bool{
		`{"a":{"b":"x","l":[1,2]}}`:   false,
		`{"a":{}}`:                    true, // nested required
		`{"a":{"b":"z"}}`:             true, // enum
		`{"a":{"b":"x","l":[1,"s"]}}`: true, // items type
	} {
		_, _, err := Structured(answer, schema)
		if (err != nil) != wantErr {
			t.Errorf("%s: err=%v, want error=%v", answer, err, wantErr)
		}
	}
	_, unchecked, _ := Structured(`{"a":{"b":"x"}}`, `{"type":"object","properties":{"n":{"minimum":1,"pattern":"x"}},"title":"t"}`)
	if !slices.Equal(unchecked, []string{"minimum", "pattern"}) {
		t.Errorf("unchecked = %v, want [minimum pattern]", unchecked)
	}
}

// Boolean schemas: false forbids any value at that spot, true allows any.
func TestStructuredAnswerBooleanSchemas(t *testing.T) {
	const schema = `{"type":"object","properties":{"x":false,"y":true,"l":{"type":"array","items":false}}}`
	for answer, wantErr := range map[string]bool{
		`{"y":{"any":1},"l":[]}`: false,
		`{"x":1}`:                true, // property false: must be absent
		`{"l":[1]}`:              true, // items false: no elements
	} {
		if _, _, err := Structured(answer, schema); (err != nil) != wantErr {
			t.Errorf("%s: err=%v, want error=%v", answer, err, wantErr)
		}
	}
	if _, _, err := Structured(`{}`, `false`); err == nil {
		t.Error("top-level false schema accepted an answer")
	}
}

// Enum numbers compare exactly: 2^53+1 is not 2^53, and 1.0 still equals 1.
func TestStructuredAnswerEnumExactNumbers(t *testing.T) {
	const schema = `{"type":"integer","enum":[9007199254740992, 1.0]}`
	for answer, wantErr := range map[string]bool{
		`9007199254740992`: false,
		`9007199254740993`: true,
		`1`:                false,
		`1 trailing`:       true,
	} {
		if _, _, err := Structured(answer, schema); (err != nil) != wantErr {
			t.Errorf("%s: err=%v, want error=%v", answer, err, wantErr)
		}
	}
}

// The span whose opener comes first is the outer one; the other pair is the fallback when it is not valid JSON.
func TestJSONSpanOuterOpenerWins(t *testing.T) {
	for name, c := range map[string][2]string{
		"array in prose with inner object": {`Here you go: [{"name":"a"}]`, `[{"name":"a"}]`},
		"object in prose":                  {`Sure: {"name":"a"} done`, `{"name":"a"}`},
		"object with nested array":         {`x {"l":[1,2]} y`, `{"l":[1,2]}`},
		"fenced block beats prose":         {"see [1]\n```json\n{\"k\":1}\n```", `{"k":1}`},
		"whole text":                       {` {"k":[1]} `, `{"k":[1]}`},
		"invalid first pair falls over":    {`[oops] {"k":1}`, `{"k":1}`},
	} {
		if got := jsonSpan(c[0]); got != c[1] {
			t.Errorf("%s: jsonSpan(%q) = %q, want %q", name, c[0], got, c[1])
		}
	}
}

func TestStructuredArrayInProseKeepsArray(t *testing.T) {
	v, _, err := Structured(`Here you go: [{"name":"a"}]`, `{"type":"array","items":{"type":"object"}}`)
	if err != nil {
		t.Fatalf("array schema rejected an array answer: %v", err)
	}
	if string(v) != `[{"name":"a"}]` {
		t.Errorf("value = %s, want the array", v)
	}
}

func TestStructuredAdditionalProperties(t *testing.T) {
	const schema = `{"type":"object","additionalProperties":false,"required":["findings"],"properties":{"findings":{"type":"array"}}}`
	if _, _, err := Structured(`{"findings":[]}`, schema); err != nil {
		t.Errorf("valid document rejected: %v", err)
	}
	if raw, notes, err := Structured(`{"findings":[],"extra":1}`, schema); err != nil || string(raw) != `{"findings":[]}` || len(notes) != 1 {
		t.Errorf("extra property not dropped and noted: %s %v %v", raw, notes, err)
	}
	const typed = `{"type":"object","additionalProperties":{"type":"integer"}}`
	if _, _, err := Structured(`{"a":"x"}`, typed); err == nil {
		t.Error("extra property of the wrong type accepted")
	}
}

func TestSchemaForDescription(t *testing.T) {
	type rec struct {
		K string `json:"k" jsonschema:"description=the kind of thing,enum=p|q"`
	}
	const want = `{"type":"object","additionalProperties":false,"required":["k"],"properties":{"k":` +
		`{"type":"string","description":"the kind of thing","enum":["p","q"]}}}`
	var a, b any
	if err := json.Unmarshal([]byte(SchemaFor[rec]()), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("got %s", SchemaFor[rec]())
	}
}

func TestSchemaForMatchesHandWritten(t *testing.T) {
	type part struct {
		ID    string   `json:"id"`
		Sev   string   `json:"sev" jsonschema:"enum=A|B"`
		Owns  []string `json:"owns,omitempty"`
		Count *int     `json:"count,omitempty"`
	}
	const want = `{"type":"object","additionalProperties":false,"required":["id","sev"],"properties":{"id":{"type":"string"},` +
		`"sev":{"type":"string","enum":["A","B"]},"owns":{"type":"array","items":{"type":"string"}},"count":{"type":"integer"}}}`
	var a, b any
	if err := json.Unmarshal([]byte(SchemaFor[part]()), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("got %s", SchemaFor[part]())
	}
}

func TestStructuredDropsUndeclaredKeys(t *testing.T) {
	const schema = `{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"array","items":` +
		`{"type":"object","additionalProperties":false,"properties":{"x":{"type":"integer"}}}}}}`
	raw, notes, err := Structured(`{"a":[{"x":1,"y":2}],"extra":1}`, schema)
	if err != nil || string(raw) != `{"a":[{"x":1}]}` {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	if want := []string{"dropped answer.a[].y", "dropped answer.extra"}; !slices.Equal(notes, want) {
		t.Errorf("notes = %v, want %v", notes, want)
	}
	if _, _, err := Structured(`{"a":[{"x":"s"}]}`, schema); err == nil {
		t.Error("type violation accepted")
	}
	if _, _, err := Structured(`{"extra":1}`, schema); err == nil {
		t.Error("missing required key accepted")
	}
}

type genBase struct {
	B string `json:"b"`
}

type GenTagged struct {
	T string `json:"t"`
}

type GenHidden struct {
	H string `json:"h"`
}

type genRec struct {
	Next *genRec `json:"next"`
}

func TestSchemaForTypes(t *testing.T) {
	type doc struct {
		genBase
		GenTagged `json:"tagged"`
		GenHidden `json:"-"`
		At        time.Time       `json:"at"`
		Data      []byte          `json:"data"`
		Any       json.RawMessage `json:"any"`
	}
	const want = `{"type":"object","additionalProperties":false,"required":["b","tagged","at","data","any"],"properties":{` +
		`"b":{"type":"string"},"tagged":{"type":"object","additionalProperties":false,"required":["t"],"properties":{"t":{"type":"string"}}},` +
		`"at":{"type":"string","format":"date-time"},"data":{"type":"string"},"any":{}}}`
	var a, b any
	if err := json.Unmarshal([]byte(SchemaFor[doc]()), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("got %s", SchemaFor[doc]())
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "genRec") {
			t.Errorf("recursive type: recover = %v", r)
		}
	}()
	SchemaFor[genRec]()
}

func TestDecode(t *testing.T) {
	type row struct {
		N int    `json:"n"`
		S string `json:"s"`
	}
	v, notes, err := Decode[row](`{"n":1,"s":"a"}`)
	if err != nil || v != (row{1, "a"}) || len(notes) != 0 {
		t.Fatalf("valid: %v %v %v", v, notes, err)
	}
	if v, notes, err = Decode[row](`{"n":2,"s":"b","x":1}`); err != nil || v.N != 2 || len(notes) != 1 || notes[0] != "dropped answer.x" {
		t.Fatalf("extra: %v %v %v", v, notes, err)
	}
	if _, _, err = Decode[row](`{"n":"no","s":"b"}`); err == nil {
		t.Fatal("type violation must error")
	}
}
