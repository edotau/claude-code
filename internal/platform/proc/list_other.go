//go:build !unix

package proc

// List has no portable source off unix: an empty table, so process sweeps are a no-op.
func List(string) ([]Info, error) { return nil, nil }
