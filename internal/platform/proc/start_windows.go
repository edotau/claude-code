package proc

import (
	"errors"
	"strconv"

	"golang.org/x/sys/windows"
)

// StartTime identifies a live Windows process by its creation time; access failures remain probe errors.
func StartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", ErrNoProcess
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return "", ErrNoProcess
	}
	if err != nil {
		return "", err
	}
	var creation, exit, kernel, user windows.Filetime
	err = windows.GetProcessTimes(h, &creation, &exit, &kernel, &user)
	if err := errors.Join(err, windows.CloseHandle(h)); err != nil {
		return "", err
	}
	if exit.HighDateTime != 0 || exit.LowDateTime != 0 {
		return "", ErrNoProcess
	}
	return strconv.FormatUint(uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime), 10), nil
}
