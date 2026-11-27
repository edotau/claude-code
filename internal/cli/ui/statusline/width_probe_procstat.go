package statusline

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parseProcStat is platform-independent so Linux's stat format is tested on every host.
func parseProcStat(stat string) (int, string, error) {
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end <= open {
		return 0, "", errors.New("invalid proc stat comm")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 5 {
		return 0, "", errors.New("short proc stat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0, "", errors.New("invalid proc stat ppid")
	}
	// tty_nr is a signed 32-bit device number, not the host platform's dev_t encoding.
	tty, err := strconv.ParseInt(fields[4], 10, 32)
	if err != nil {
		return 0, "", fmt.Errorf("invalid proc stat tty_nr: %w", err)
	}
	return ppid, linuxTTYName(uint32(tty)), nil
}

// linuxTTYName decodes Linux's new_encode_dev layout independently of the host OS.
func linuxTTYName(dev uint32) string {
	major := (dev >> 8) & 0xfff
	minor := (dev & 0xff) | ((dev >> 12) & 0xfff00)
	switch {
	case major >= 136 && major <= 143:
		return fmt.Sprintf("pts/%d", (major-136)*256+minor)
	case major == 4 && minor < 64:
		return fmt.Sprintf("tty%d", minor)
	case major == 4:
		return fmt.Sprintf("ttyS%d", minor-64)
	case major == 5 && minor == 1:
		return "console"
	case major == 5 && minor == 0:
		return "tty"
	case major == 188:
		return fmt.Sprintf("ttyUSB%d", minor)
	case major == 166:
		return fmt.Sprintf("ttyACM%d", minor)
	default:
		return ""
	}
}
