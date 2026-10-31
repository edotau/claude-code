//go:build !linux && !darwin

package agent

func runtimeDirPath() (string, error) { return "", errNoRuntimeDir }

func backingCheck(string) error { return errNoRuntimeDir }
