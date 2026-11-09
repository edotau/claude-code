package databricks

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintDiffScalarToEmptyArray(t *testing.T) {
	var out bytes.Buffer
	if !printDiff(&out, []byte(`{"x":"value"}`), []byte(`{"x":[]}`)) {
		t.Fatal("scalar-to-empty-array transition reported no change")
	}
	for _, want := range []string{`-   "x": "value"`, `+   "x": []`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("diff missing %q:\n%s", want, out.String())
		}
	}
}
