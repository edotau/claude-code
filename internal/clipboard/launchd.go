package clipboard

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"text/template"
)

// launchLabel names the macOS launchd agent that serves the clipboard bridge on demand.
const launchLabel = "com.edotau.claude-code.clip"

// plistTmpl is the socket-activated agent: launchd accepts each connection and hands it over as stdin/stdout.
var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"xml": xmlText}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{xml .Bin}}</string>
		<string>clip</string>
		<string>--serve</string>
	</array>
	<key>inetdCompatibility</key>
	<dict>
		<key>Wait</key>
		<false/>
	</dict>
	<key>Sockets</key>
	<dict>
		<key>Listeners</key>
		<dict>
			<key>SockPathName</key>
			<string>{{xml .Sock}}</string>
			<key>SockPathMode</key>
			<integer>384</integer>
		</dict>
	</dict>
</dict>
</plist>
`))

// xmlText escapes s for a plist <string> body, so a path with & or < stays well-formed.
func xmlText(s string) (string, error) {
	var b bytes.Buffer
	err := xml.EscapeText(&b, []byte(s))
	return b.String(), err
}

// launchSocket is the Unix socket launchd listens on, the target of the client's RemoteForward.
func launchSocket(home string) string { return filepath.Join(home, ".claude", "run", "clip.sock") }

// launchPlistPath is the per-user LaunchAgents file for the bridge.
func launchPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

// renderPlist returns the launchd agent definition with absolute paths.
func renderPlist(bin, home string) ([]byte, error) {
	var buf bytes.Buffer
	err := plistTmpl.Execute(&buf, struct{ Bin, Sock string }{bin, launchSocket(home)})
	return buf.Bytes(), err
}

// InstallLaunchAgent installs the on-demand clipboard bridge agent on macOS; elsewhere a silent no-op.
func InstallLaunchAgent(w io.Writer, bin string, dryRun bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return installLaunchAgentAt(w, home, bin, os.Getuid(), dryRun)
}

// installLaunchAgentAt is InstallLaunchAgent without the darwin gate; an unchanged plist skips the reload.
func installLaunchAgentAt(w io.Writer, home, bin string, uid int, dryRun bool) error {
	want, err := renderPlist(bin, home)
	if err != nil {
		return err
	}
	plist := launchPlistPath(home)
	domain := "gui/" + strconv.Itoa(uid)
	if have, err := os.ReadFile(plist); err == nil && bytes.Equal(have, want) {
		if launchctl("print", domain+"/"+launchLabel) == nil {
			fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "current: "+plist)
			return nil
		}
		return bootstrapAgent(w, home, domain, plist, dryRun) // plist current but not loaded
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "would write "+plist+" (skipped: --dry-run)")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(launchSocket(home)), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, want, 0o644); err != nil {
		return err
	}
	_ = launchctl("bootout", domain+"/"+launchLabel) // not loaded yet is fine
	if err := launchctl("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "wrote "+plist)
	return nil
}

// bootstrapAgent loads an already-current plist that launchd does not have (booted out, failed bootstrap).
func bootstrapAgent(w io.Writer, home, domain, plist string, dryRun bool) error {
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "would load "+plist+" (skipped: --dry-run)")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(launchSocket(home)), 0o755); err != nil {
		return err
	}
	if err := launchctl("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "loaded "+plist)
	return nil
}

// launchctl runs launchctl with args, folding its output into the error.
func launchctl(args ...string) error {
	out, err := exec.CommandContext(context.Background(), "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// launchDoctorDetail reports the macOS bridge agent: plist present and the listening socket path exists.
func launchDoctorDetail() (ok bool, detail string) {
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(launchPlistPath(home)); err != nil {
		return false, "clip bridge agent absent (run claude-code install)"
	}
	if _, err := os.Stat(launchSocket(home)); err != nil {
		return false, "clip bridge agent installed but " + launchSocket(home) + " is missing (launchctl bootstrap)"
	}
	return true, "clip bridge agent at " + launchSocket(home)
}
