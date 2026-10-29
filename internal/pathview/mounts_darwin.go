package pathview

import (
	"context"
	"os/exec"
	"time"
)

func readMounts() []Mount {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "mount").Output()
	if err != nil {
		return nil
	}
	return parseDarwinMount(string(out))
}
