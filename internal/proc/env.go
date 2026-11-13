package proc

import "os"

// Setenv sets a process-wide env var; process-wide state lives here, per-run values belong on the run.
func Setenv(key, value string) error { return os.Setenv(key, value) }

// Unsetenv unsets a process-wide env var; process-wide state lives here, per-run values belong on the run.
func Unsetenv(key string) error { return os.Unsetenv(key) }

// Chdir changes the process-wide working directory; process-wide state lives here, per-run values belong on the run.
func Chdir(dir string) error { return os.Chdir(dir) }
