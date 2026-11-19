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
		"go test ./internal/x -run 'A|B$'":         true,
		"go vet ./a && GOOS=windows go vet ./b":    true,
		"claude-code doctor --prompts":             true,
		"go test ./$(id)":                          false,
		"go test -run $(id) ./x":                   false,
		"claude-code doctor $(id)":                 false,
		"claude-code agents diff --check > /tmp/x": false,
		"go test ./x `id`":                         false,
		"go test ./../other":                       false,
		"GOOS=$(id) go test ./x":                   false,
		"claude-code doctor --secrets --fix":       false,
		"claude-code agents diff --check --base x": false,
		"go test ./x; id":                          false,
		"go test ./x\nid":                          false,
	} {
		if err := VerifyShape(cmd); (err == nil) != allowed {
			t.Errorf("VerifyShape(%q) = %v, allowed %v", cmd, err, allowed)
		}
	}
}
