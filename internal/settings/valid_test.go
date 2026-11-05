package settings

import (
	"strings"
	"testing"
)

// TestCheckReportsSyntaxLocationAndSchemaErrors: valid passes, unknown keys pass, a bad value or bad JSON is named.
func TestCheckReportsSyntaxLocationAndSchemaErrors(t *testing.T) {
	for in, want := range map[string]string{
		`{"model": "opus", "zzMine": [1]}`:  "",
		`{"cleanupPeriodDays": "7"}`:        "cleanupPeriodDays: want a number",
		"{\n  \"model\": \"x\",\n  oops\n}": "line 3 col 4",
		`[]`:                                "cannot unmarshal array",
		`null`:                              "not a JSON object",
	} {
		err := Check([]byte(in))
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want %q", in, err, want)
		}
	}
}
