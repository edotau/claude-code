package wire

import (
	"slices"
	"testing"
)

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
