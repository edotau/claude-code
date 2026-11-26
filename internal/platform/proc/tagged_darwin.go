package proc

import (
	"encoding/binary"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// tagged returns this user's pids whose initial env holds tag, read from procargs2 after argv.
func tagged(tag string) ([]int, error) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var pids []int
	for i := range all {
		if pid := int(all[i].Proc.P_pid); pid > 1 && pid != self && hasTag(pid, tag) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func hasTag(pid int, tag string) bool {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	return err == nil && procArgsEnvHas(raw, tag+"=")
}

// envUnreadable reports pid's env cannot be seen: the sysctl fails, or Apple platform binaries get an empty env.
func envUnreadable(pid int) bool {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	return err != nil || len(procArgsEnv(raw)) == 0
}

// procArgsEnvHas parses procargs2 (argc, exec path padded to 8 bytes, argc args, env to the end) for an env entry
// starting want; an empty arg counts as one, and an empty env entry is skipped, not the end.
func procArgsEnvHas(raw []byte, want string) bool {
	return slices.ContainsFunc(procArgsEnv(raw), func(kv string) bool { return strings.HasPrefix(kv, want) })
}

// procArgsEnv is the non-empty env entries of a procargs2 blob; none when it is malformed or the env is hidden.
func procArgsEnv(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	argc := int(binary.NativeEndian.Uint32(raw))
	path, _, ok := strings.Cut(string(raw[4:]), "\x00")
	start := 4 + (len(path)+1+7)&^7
	if !ok || start > len(raw) {
		return nil
	}
	fields := strings.Split(string(raw[start:]), "\x00")
	if argc > len(fields) {
		return nil
	}
	var env []string
	for _, kv := range fields[argc:] {
		if kv != "" {
			env = append(env, kv)
		}
	}
	return env
}
