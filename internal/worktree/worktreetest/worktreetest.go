// Package worktreetest provides dependency-free process scan stubs for tests.
package worktreetest

// StubProcessScanForTest makes the process scan report nothing; call from TestMain.
func StubProcessScanForTest[P, C any](in *func(string) ([]P, error), up *func(int) (int, error),
	procs, agents *func() ([]C, error)) {
	*agents = nil
	*in = func(string) ([]P, error) { return nil, nil }
	*up = func(int) (int, error) { return 0, nil }
	*procs = func() ([]C, error) { return nil, nil }
}
