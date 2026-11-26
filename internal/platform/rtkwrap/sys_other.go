//go:build !unix

package rtkwrap

import "errors"

// sessionID has no Windows meaning: -1 never matches a pid, so nothing wraps.
func sessionID() int { return -1 }

func execve(string, []string, []string) error { return errors.New("rtk shims run only on unix") }
