package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintRedactsCredentialSegmentNames(t *testing.T) {
	set := map[string]string{"DATA_PATH": "/visible"}
	for _, k := range []string{"FOO_HEADERS", "MY_AUTH", "DB_CREDENTIALS", "DATABRICKS_PAT", "X_BEARER", "X_AUTH_Y"} {
		set[k] = "s3cr3t-value"
	}
	var b bytes.Buffer
	(&Plan{Set: set}).Print(&b)
	out := b.String()
	if strings.Contains(out, "s3cr3t-value") {
		t.Errorf("Print leaked a credential-shaped value:\n%s", out)
	}
	if !strings.Contains(out, "DATA_PATH=/visible") {
		t.Errorf("benign DATA_PATH was redacted:\n%s", out)
	}
}
