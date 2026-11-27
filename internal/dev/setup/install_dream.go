package setup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"text/template"

	"github.com/edotau/claude-code/internal/platform/launchd"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// installDreamAgent is a var so tests never bootstrap the developer's real launchd agent.
var installDreamAgent = dreamLaunchAgent

// dreamLogMax bounds dream-launchd.log: launchd appends forever, so install truncates past this size.
const dreamLogMax = 1 << 20

const dreamLabel = "com.edotau.claude-code.dream"

// dreamEvery matches dreamRetry, so a failed dream retries on schedule and a due one never waits past 6 h.
const dreamEvery = 6 * 60 * 60

var dreamPlistTmpl = template.Must(template.New("dream").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Bin}}</string>
		<string>memory</string>
		<string>update</string>
		<string>--dream</string>
		<string>--trigger</string>
		<string>schedule</string>
		<string>--cwd</string>
		<string>{{.Cwd}}</string>
	</array>
	<key>StartInterval</key><integer>{{.Every}}</integer>
	<key>RunAtLoad</key><false/>
	<key>ProcessType</key><string>Background</string>
	<key>LowPriorityIO</key><true/>
	<key>StandardOutPath</key><string>{{.Log}}</string>
	<key>StandardErrorPath</key><string>{{.Log}}</string>
</dict>
</plist>
`))

// renderDreamPlist fills the template with XML-escaped values.
func renderDreamPlist(bin, cwd, logPath string) ([]byte, error) {
	vals := map[string]string{"Label": dreamLabel, "Bin": bin, "Cwd": cwd, "Log": logPath}
	for k, v := range vals {
		esc, err := launchd.XMLText(v)
		if err != nil {
			return nil, err
		}
		vals[k] = esc
	}
	var buf bytes.Buffer
	err := dreamPlistTmpl.Execute(&buf, struct{ Label, Bin, Cwd, Log, Every string }{
		vals["Label"], vals["Bin"], vals["Cwd"], vals["Log"], strconv.Itoa(dreamEvery)})
	return buf.Bytes(), err
}

// dreamLaunchAgent runs `memory update --dream --trigger schedule` every dreamEvery seconds on macOS (self-skips).
func dreamLaunchAgent(w io.Writer, bin string, dryRun bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(paths.MemoryStateDir(), "dream-launchd.log")
	body, err := renderDreamPlist(bin, paths.ConfigDir(), logPath)
	if err != nil {
		return err
	}
	return launchd.Install(w, launchd.Agent{
		Label:  dreamLabel,
		Plist:  filepath.Join(home, "Library", "LaunchAgents", dreamLabel+".plist"),
		Domain: "gui/" + strconv.Itoa(os.Getuid()),
		Body:   body,
		Prep:   func() error { return prepDreamLog(logPath) },
	}, dryRun)
}

func prepDreamLog(logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > dreamLogMax {
		return os.Truncate(logPath, 0)
	}
	return nil
}
