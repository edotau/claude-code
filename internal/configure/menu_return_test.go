package configure

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRunMenuReturnKeyState(t *testing.T) {
	const good = "CONFIGURE_RETURN_GOOD"
	const other = "CONFIGURE_RETURN_OTHER"
	const prior = "synthetic-prior"
	const valid = "synthetic-staged"
	const invalid = "synthetic\ninvalid"
	for _, tc := range []struct {
		name       string
		save       bool
		otherValue string
		wantRC     int
		failed     bool
	}{
		{name: "all_stored", save: true, otherValue: valid},
		{name: "discard", otherValue: valid},
		{name: "partial_failure", save: true, otherValue: invalid, wantRC: 1, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := menuTestEnv(t)
			vaulttest.Use(t)
			t.Setenv(good, prior)
			t.Setenv(other, "")
			if err := os.Unsetenv(other); err != nil {
				t.Fatal("could not unset synthetic environment variable")
			}
			originalPicker, originalConfirm := pickMenu, saveConfirmed
			t.Cleanup(func() { pickMenu, saveConfirmed = originalPicker, originalConfirm })
			var out bytes.Buffer
			d := &dialog{reg: reg, out: &out}
			calls, confirmations := 0, 0
			pickMenu = flatPick(func(string, [][2]string, int) (int, error) {
				calls++
				if !d.stage(good, valid) || !d.stage(other, tc.otherValue) {
					t.Fatal("could not stage synthetic keys")
				}
				return -1, nil
			})
			saveConfirmed = func(string) bool {
				confirmations++
				return tc.save
			}
			if rc := runMenu(d); rc != tc.wantRC {
				t.Fatalf("return code = %d, want %d", rc, tc.wantRC)
			}
			if calls != 1 || confirmations != 1 {
				t.Fatal("expected exactly one picker call and one leave confirmation")
			}
			if os.Getenv(good) != prior {
				t.Error("successful or discarded key did not restore its prior environment")
			}
			if _, pending := d.pending[good]; pending {
				t.Error("successful or discarded key remained pending")
			}
			if tc.failed {
				if _, pending := d.pending[other]; !pending || len(d.pending) != 1 {
					t.Error("only the failed key should remain pending")
				}
				if os.Getenv(other) != invalid {
					t.Error("failed key did not retain its staged environment")
				}
				if !strings.Contains(out.String(), other+": not saved:") {
					t.Error("failure diagnostic did not identify the failed key by name")
				}
			} else {
				if len(d.pending) != 0 {
					t.Error("pending should be empty after success or discard")
				}
				if _, present := os.LookupEnv(other); present {
					t.Error("previously absent environment variable was not removed")
				}
			}
			names, err := secrets.NamesErr()
			if err != nil {
				t.Fatal("could not inspect isolated vault names")
			}
			if slices.Contains(names, good) != tc.save {
				t.Error("successful key vault presence does not match save choice")
			}
			if slices.Contains(names, other) != (tc.save && !tc.failed) {
				t.Error("other key vault presence does not match save outcome")
			}
			if !tc.save && len(names) != 0 {
				t.Error("discard unexpectedly wrote a vault secret")
			}
		})
	}
}
