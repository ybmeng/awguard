package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// launchdLabel identifies the std service to launchd.
const launchdLabel = "com.std.bgservices"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
        <string>-dir</string>
        <string>%s</string>
        <string>-interval</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`

func requireDarwin() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service control requires macOS (launchd); this is %s", runtime.GOOS)
	}
	return nil
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func logPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "stdd.log"), nil
}

// serviceTarget is the launchctl domain-qualified name of the service for the
// current user's GUI session.
func serviceTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v (%s)", args[0], err, string(out))
	}
	if len(out) > 0 {
		fmt.Print(string(out))
	}
	return nil
}

// installService writes the LaunchAgent plist pointing at the current binary
// and bootstraps it into the user's launchd session.
func installService(dir string, interval time.Duration) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve -dir: %w", err)
	}

	plist, err := plistPath()
	if err != nil {
		return err
	}
	logs, err := logPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logs), 0o755); err != nil {
		return err
	}

	content := fmt.Sprintf(plistTemplate, launchdLabel, exe, absDir, interval, logs, logs)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}

	// Re-bootstrap cleanly if a previous version is loaded.
	_ = exec.Command("launchctl", "bootout", serviceTarget()).Run()
	if err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist); err != nil {
		return err
	}
	fmt.Printf("stdd: installed %s\n  binary: %s\n  dir:    %s\n  logs:   %s\n", launchdLabel, exe, absDir, logs)
	return nil
}

// uninstallService stops the service and removes its plist.
func uninstallService() error {
	if err := requireDarwin(); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", serviceTarget()).Run()
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("stdd: uninstalled %s\n", launchdLabel)
	return nil
}

// startService bootstraps the already-installed plist.
func startService() error {
	if err := requireDarwin(); err != nil {
		return err
	}
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); err != nil {
		return fmt.Errorf("service not installed (run: stdd install -dir DIR): %w", err)
	}
	return launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist)
}

// stopService unloads the service without removing the plist.
func stopService() error {
	if err := requireDarwin(); err != nil {
		return err
	}
	return launchctl("bootout", serviceTarget())
}

// restartService kills and relaunches the running service.
func restartService() error {
	if err := requireDarwin(); err != nil {
		return err
	}
	return launchctl("kickstart", "-k", serviceTarget())
}

// serviceStatus prints launchd's view of the service.
func serviceStatus() error {
	if err := requireDarwin(); err != nil {
		return err
	}
	return launchctl("print", serviceTarget())
}
