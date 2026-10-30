package textutil

import (
	"encoding/json"
	"testing"
)

func TestStripJSONCHandlesCommentsStringsAndTrailingCommas(t *testing.T) {
	in := `{
  // a line comment
  "a": "value with // not a comment and /* not a block */",
  "b": [1, 2, 3,],
  /* a block
     comment */
  "c": {"nested": true,},
}`
	var out map[string]any
	if err := json.Unmarshal(StripJSONC([]byte(in)), &out); err != nil {
		t.Fatalf("stripped JSONC did not parse: %v\n%s", err, StripJSONC([]byte(in)))
	}
	if out["a"] != "value with // not a comment and /* not a block */" {
		t.Errorf("string content was mangled: %v", out["a"])
	}
	if out["b"].([]any) == nil || len(out["b"].([]any)) != 3 {
		t.Errorf("array trailing comma not stripped cleanly: %v", out["b"])
	}
	nested, ok := out["c"].(map[string]any)
	if !ok || nested["nested"] != true {
		t.Errorf("object trailing comma not stripped cleanly: %v", out["c"])
	}
}
