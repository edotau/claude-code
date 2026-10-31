package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// decodeLines skips blank lines and logs non-JSON output without losing subsequent events.
func decodeLines(br *bufio.Reader, lg eventLog, fn func(raw []byte) (stop bool, err error)) error {
	for {
		raw, readErr := br.ReadBytes('\n')
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 {
			if !json.Valid(raw) {
				lg.emit("raw", string(raw))
			} else {
				stop, err := fn(raw)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}
