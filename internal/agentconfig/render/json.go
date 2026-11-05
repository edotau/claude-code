package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ReadExisting reads path; a missing file is (nil, false, nil), any other read failure is an error.
func ReadExisting(path string) (raw []byte, existed bool, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// CommitJSONObject reads path as a JSON object, lets mutate edit it, and commits the result. An unreadable or
// non-object file is skipped with a "label: skipped" note and left untouched; mutate returning false writes nothing.
func CommitJSONObject(path string, o RenderOptions, out io.Writer, label string, mutate func(obj map[string]any, existed bool) bool) (bool, error) {
	raw, existed, err := ReadExisting(path)
	if err != nil {
		fmt.Fprintf(out, "%s: skipped: cannot read %s: %v\n", label, path, err)
		return false, nil
	}
	obj := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &obj); err != nil {
			fmt.Fprintf(out, "%s: skipped: %s is not plain JSON: %v\n", label, path, err)
			return false, nil
		}
		if obj == nil { // "null" decodes without error but leaves obj nil, not an empty object
			fmt.Fprintf(out, "%s: skipped: %s is not plain JSON: not an object\n", label, path)
			return false, nil
		}
	}
	if !mutate(obj, existed) {
		return false, nil
	}
	body, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	return CommitFile(path, raw, append(body, '\n'), existed, o, out, label)
}
