package route

import "testing"

func TestParse(t *testing.T) {
	for _, bad := range []string{"databricks:", "nope", "databricks:a:b", "default", "first-party", "local", "anthropic:", ":dev"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	for in, want := range map[string][2]string{"": {}, " anthropic ": {"anthropic"}, "github:personal": {"github", "personal"},
		"databricks": {"databricks"}, "databricks:dev": {"databricks", "dev"}} {
		provider, profile, err := Parse(in)
		if err != nil || [2]string{provider, profile} != want {
			t.Errorf("Parse(%q) = %q, %q, %v; want %q", in, provider, profile, err, want)
		}
	}
}
