//go:build !linux && !darwin

package copilotreg

func runtimeDirPath() (string, error) { return "", errNoRuntimeDir }

func backingCheck(string) error { return errNoRuntimeDir }
