package agent

import (
	"encoding/json"
	"testing"
)

// A property named "properties" is data, not a schema: only real object schemas get required/additionalProperties.
func TestCodexStrictWalksSchemaKeywordsOnly(t *testing.T) {
	var v any
	src := `{"type":"object","properties":{"properties":{"type":"string"},"items":{"type":"array",` +
		`"items":{"type":"object","properties":{"n":{"type":"integer"}}}}},"$defs":{"d":{"properties":{"z":{}}}}}`
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(codexStrict(v))
	want := `{"$defs":{"d":{"additionalProperties":false,"properties":{"z":{}},"required":["z"]}},` +
		`"additionalProperties":false,"properties":{"items":{"items":{"additionalProperties":false,` +
		`"properties":{"n":{"type":"integer"}},"required":["n"],"type":"object"},"type":"array"},` +
		`"properties":{"type":"string"}},"required":["items","properties"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("codexStrict =\n%s\nwant\n%s", got, want)
	}
}
