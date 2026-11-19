package plans

import "testing"

func TestVerifyShape(t *testing.T) {
	for cmd, allowed := range map[string]bool{
		"go test ./pkg":                            true,
		"rtk go test ./x -run T":                   true,
		"GOOS=linux go build ./...":                true,
		"claude-code agents diff --check --path a": true,
		"claude-code doctor":                       true,
		"make test":                                false,
		"go test ./... | tee x":                    false,
		"go test -o /tmp/x ./pkg":                  false,
		"curl x":                                   false,
	} {
		if err := VerifyShape(cmd); (err == nil) != allowed {
			t.Errorf("VerifyShape(%q) = %v, allowed %v", cmd, err, allowed)
		}
	}
}
