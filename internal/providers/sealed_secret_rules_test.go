package providers

import (
	"strings"
	"testing"
)

const dbxHost = "https://example.cloud.databricks.com"

func patRow(host string) string {
	return `{"preset":"databricks","host":"` + host + `","auth":{"authType":"pat"}}`
}

func TestValidateRejectsCollidingSealedSecretNames(t *testing.T) {
	doc := `{"providers":{"a-b":` + patRow(dbxHost) + `,"a_b":` + patRow(dbxHost) + `}}`
	err := Validate([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "a-b") || !strings.Contains(err.Error(), "a_b") {
		t.Fatalf("Validate = %v, want an error naming a-b and a_b", err)
	}
}

func TestValidateAcceptsDistinctSealedSecretNames(t *testing.T) {
	doc := `{"providers":{"a-b":` + patRow(dbxHost) + `,"c_d":` + patRow(dbxHost) + `}}`
	if err := Validate([]byte(doc)); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}

func m2mBaseRow(base string) string {
	return `{"kind":"anthropic","base_url":"` + base + `/serving-endpoints/anthropic",` +
		`"auth":{"type":"bearer","authType":"oauth-m2m","clientId":"sp-client"}}`
}

func TestValidateRejectsPlainHTTPSealedHost(t *testing.T) {
	for _, doc := range []string{
		`{"providers":{"m2m-http":` + m2mBaseRow("http://example.cloud.databricks.com") + `}}`,
		`{"providers":{"pat-http":{"preset":"databricks","host":"http://example.cloud.databricks.com","auth":{"authType":"pat"}}}}`,
	} {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("Validate accepted plain http: %s", doc)
		}
	}
}

func TestValidateAcceptsHTTPSAndLoopbackHTTPSealedHost(t *testing.T) {
	for _, base := range []string{dbxHost, "http://127.0.0.1:8080", "http://localhost:9"} {
		doc := `{"providers":{"m2m-ok":` + m2mBaseRow(base) + `}}`
		if err := Validate([]byte(doc)); err != nil {
			t.Errorf("Validate(%s) = %v", base, err)
		}
	}
}

func TestAuthHostRejectsPlainHTTPNonLoopback(t *testing.T) {
	p := &Provider{Name: "x", BaseURL: "http://example.cloud.databricks.com/serving-endpoints/anthropic",
		Auth: Auth{Type: AuthBearer, AuthType: AuthOAuthM2M, ClientID: "c"}}
	if h := AuthHost(p); h != "" {
		t.Errorf("AuthHost = %q, want empty for plain http non-loopback", h)
	}
}
