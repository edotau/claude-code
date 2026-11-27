package statusline

import "testing"

func TestParseProcStat(t *testing.T) {
	for _, tc := range []struct {
		name, stat string
		ppid       int
		tty        string
	}{
		{"no tty", "42 (worker) S 21 42 42 0 0", 21, ""},
		{"spaces", "42 (worker with spaces) S 21 42 42 34819 0", 21, "pts/3"},
		{"parens", "42 (worker ) (nested)) S 21 42 42 34817 0", 21, "pts/1"},
		{"unmatched open", "42 ((worker) S 21 42 42 34816 0", 21, "pts/0"},
		{"embedded newline", "42 (worker\nname) S 21 42 42 1025 0", 21, "tty1"},
		{"root parent", "1 (init) S 0 1 1 0 0", 0, ""},
		{"serial", "42 (worker) S 21 42 42 1088 0", 21, "ttyS0"},
		{"next pts major", "42 (worker) S 21 42 42 35073 0", 21, "pts/257"},
		{"high minor bits", "42 (worker) S 21 42 42 1096705 0", 21, "ttyUSB257"},
		{"signed device", "42 (worker) S 21 42 42 -2147435520 0", 21, "ttyUSB524288"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ppid, tty, err := parseProcStat(tc.stat)
			if err != nil || ppid != tc.ppid || tty != tc.tty {
				t.Errorf("parse = (%d, %q, %v), want (%d, %q, nil)", ppid, tty, err, tc.ppid, tc.tty)
			}
		})
	}
}

func TestParseProcStatMalformed(t *testing.T) {
	for _, stat := range []string{
		"", "42 worker S 21 42 42 0", "42 (worker S 21 42 42 0",
		"42 )worker( S 21 42 42 0", "42 (worker) S 21 42 42",
		"42 (worker) S bad 42 42 0", "42 (worker) S -1 42 42 0",
		"42 (worker) S 21 42 42 bad", "42 (worker) S 21 42 42 2147483648",
		"42 (worker) S 21 42 42 -2147483649",
	} {
		if _, _, err := parseProcStat(stat); err == nil {
			t.Errorf("parseProcStat(%q): expected an error", stat)
		}
	}
}

func TestLinuxTTYName(t *testing.T) {
	for _, tc := range []struct {
		dev  uint32
		want string
	}{
		{0, ""}, {0x8800, "pts/0"}, {0x8fff, "pts/2047"},
		{0x400, "tty0"}, {0x43f, "tty63"}, {0x440, "ttyS0"}, {0x441, "ttyS1"},
		{0x500, "tty"}, {0x501, "console"}, {0x502, ""},
		{0xbc02, "ttyUSB2"}, {0xa603, "ttyACM3"}, {0xffff, ""},
	} {
		if got := linuxTTYName(tc.dev); got != tc.want {
			t.Errorf("linuxTTYName(%#x) = %q, want %q", tc.dev, got, tc.want)
		}
	}
}
