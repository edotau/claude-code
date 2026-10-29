package pathview

import "os"

func readMounts() []Mount {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseMountinfo(string(b))
}
