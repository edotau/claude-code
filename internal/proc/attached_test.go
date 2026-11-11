package proc

import (
	"errors"
	"os/exec"
	"testing"
)

func TestRunAttachedExitError(t *testing.T) {
	needSh(t)
	cancelled, err := RunAttached([]string{"sh", "-c", "exit 3"})
	var ee *exec.ExitError
	if cancelled || err == nil || !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("exit 3 = cancelled %v err %v, want false and exit status 3", cancelled, err)
	}
	if c, err := RunAttached([]string{"true"}); c || err != nil {
		t.Errorf("true = %v,%v", c, err)
	}
	if _, err := RunAttached(nil); err == nil {
		t.Errorf("empty argv must error")
	}
}

// The child interrupts this test process and then itself: the parent survives (signal swallowed) and the child's
// own SIGINT reads as cancelled.
func TestRunAttachedSignalCancels(t *testing.T) {
	needSh(t)
	cancelled, err := RunAttached([]string{"sh", "-c", "kill -INT $PPID; sleep 0.2; kill -INT $$; sleep 5"})
	if !cancelled || err != nil {
		t.Errorf("signalled child = cancelled %v err %v, want true,nil", cancelled, err)
	}
}

func needSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}
