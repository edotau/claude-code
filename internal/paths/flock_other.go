//go:build !unix

package paths

import "os"

func lock(*os.File) error   { return nil }
func unlock(*os.File) error { return nil }

func tryLock(*os.File) bool { return true }
