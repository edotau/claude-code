package oauth

import "testing"

func TestNormHostHTTPOnlyOnLoopback(t *testing.T) {
	ok := []string{"https://example.cloud.databricks.com", "http://127.0.0.1:8080", "http://localhost:9", "http://[::1]:7"}
	bad := []string{"http://example.cloud.databricks.com", "http://10.0.0.5", "http://example.cloud.databricks.com:8080/"}
	for _, h := range ok {
		if _, err := normHost(h); err != nil {
			t.Errorf("normHost(%q) = %v", h, err)
		}
	}
	for _, h := range bad {
		if _, err := normHost(h); err == nil {
			t.Errorf("normHost(%q) accepted plain http non-loopback", h)
		}
	}
}
