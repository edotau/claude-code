package proc

import (
	"os"
	"testing"
)

// The guard must recognise the running test binary, or a detached relaunch fork-bombs the suite.
func TestIsSelfUnderTest(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !IsSelfUnderTest(self) {
		t.Fatal("IsSelfUnderTest(os.Executable()) = false under go test")
	}
	if SpawnDetached(self, []string{"-test.run=^$"}, "", nil) {
		t.Fatal("SpawnDetached relaunched the test binary")
	}
	if IsSelfUnderTest("/bin/sh") {
		t.Fatal("IsSelfUnderTest(/bin/sh) = true")
	}
}
