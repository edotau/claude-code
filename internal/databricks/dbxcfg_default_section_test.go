package databricks

import (
	"os"
	"path/filepath"
	"testing"
)

func candidatesOf(t *testing.T, cfg string) []Candidate {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestHeaderlessKeysBelongToDefault(t *testing.T) {
	cs := candidatesOf(t, "host = https://x.cloud.databricks.com\ntoken = t\n")
	if len(cs) != 1 || cs[0].Name != "DEFAULT" || cs[0].Skip != defaultOnlySkip {
		t.Fatalf("headerless cfg: want the DEFAULT skip row, got %+v", cs)
	}
}

func TestEmptyDefaultHeaderIsNoDefault(t *testing.T) {
	if cs := candidatesOf(t, "[DEFAULT]\n"); len(cs) != 0 {
		t.Fatalf("empty [DEFAULT] must not yield a skip row, got %+v", cs)
	}
}
