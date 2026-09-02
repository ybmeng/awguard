package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"stdtools/go/std/bg_services/botnetsvc"
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
        <string>-botnet-addr</string>
        <string>%s</string>
        <string>-botnet-db</string>
        <string>%s</string>
        <string>-automations-repo</string>
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

// waitForServiceGone blocks until launchd no longer knows the service, or the
// deadline passes. Returning on timeout is deliberate: bootstrap reports a far
// better error than anything invented here.
func waitForServiceGone(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exec.Command("launchctl", "print", serviceTarget()).Run() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// botnetAddrInstallable refuses an install that would produce a daemon which can
// never bind. Supervise would restart it forever and say so only in the log, so
// the one moment the user is watching is the right place to catch it.
//
// This is checked here rather than in Verify on purpose: Verify is hermetic and
// must stay safe to run anywhere, and a bind probe is a real resource touch.
func botnetAddrInstallable(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
		return nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		// Not a conflict we can diagnose from here; let the service report it.
		return nil
	}
	// Reinstalling over ourselves is fine, since installService boots the old
	// agent out first. But that only applies when the installed agent serves
	// THIS address: an agent on some other port is not what is holding this one,
	// and treating it as such would wave through a genuine conflict.
	loaded := exec.Command("launchctl", "print", serviceTarget()).Run() == nil
	if loaded && installedBotnetAddr() == addr {
		return nil
	}
	return fmt.Errorf(
		"%s is already held by another process (a hand-run botnetd?) — stop it, or install with -botnet-addr on a free port",
		addr,
	)
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
// and bootstraps it into the user's launchd session. The botnet address and
// database and the automations repo are baked into the plist as flags: a
// launchd agent does not inherit the shell environment, so BOTNET_ADDR /
// BOTNET_DB / AUTOMATIONS_REPO would not reach it.
func installService(dir string, interval time.Duration, botnetAddr, botnetDB, automationsRepo string) error {
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

	if err := botnetAddrInstallable(botnetAddr); err != nil {
		return err
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

	content := fmt.Sprintf(plistTemplate, launchdLabel, exe, absDir, interval, botnetAddr, botnetDB, automationsRepo, logs, logs)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}

	// Re-bootstrap cleanly if a previous version is loaded. bootout returns
	// before the service is actually gone, and bootstrapping into a domain that
	// still holds it fails with "Input/output error" — which used to leave the
	// old service booted out and no new one installed, i.e. worse than before
	// the command ran. Wait for it to disappear first.
	_ = exec.Command("launchctl", "bootout", serviceTarget()).Run()
	waitForServiceGone(5 * time.Second)
	if err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist); err != nil {
		return err
	}
	repoLine := automationsRepo
	if repoLine == "" {
		repoLine = "(none — zero automations)"
	}
	fmt.Printf("stdd: installed %s\n  binary: %s\n  dir:    %s\n  botnet: http://%s\n  automations repo: %s\n  logs:   %s\n", launchdLabel, exe, absDir, botnetAddr, repoLine, logs)
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

// serviceStatus prints launchd's view of the service and whether the botnet
// address is answering. These are two independent facts — the agent can be
// loaded while botnet crash-loops, and a hand-run botnetd can hold the port
// with no agent installed at all — so both are always reported and the probe
// never claims to know which process owns the port. A botnet that is down is
// printed, not returned as an error.
func serviceStatus() error {
	if err := requireDarwin(); err != nil {
		return err
	}
	printErr := launchctl("print", serviceTarget())

	addr := installedBotnetAddr()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/v1/health")
	if err != nil {
		fmt.Printf("stdd: botnet not answering on http://%s\n", addr)
	} else {
		resp.Body.Close()
		fmt.Printf("stdd: botnet answering on http://%s — that port is taken, so a second listener (./botnetd by hand) cannot bind it\n", addr)
	}
	return printErr
}

// installedBotnetAddr recovers the -botnet-addr the installed plist runs with,
// falling back to the built-in default when there is no plist or no such flag.
// stdd authors that file itself, so scanning its <string> values is enough —
// no XML parser needed.
func installedBotnetAddr() string {
	plist, err := plistPath()
	if err != nil {
		return botnetsvc.DefaultAddr()
	}
	data, err := os.ReadFile(plist)
	if err != nil {
		return botnetsvc.DefaultAddr()
	}
	values := plistStrings(string(data))
	for i, v := range values {
		if v == "-botnet-addr" && i+1 < len(values) {
			return values[i+1]
		}
	}
	return botnetsvc.DefaultAddr()
}

// plistStrings returns the contents of every <string> element in the plist, in
// document order.
func plistStrings(plist string) []string {
	var out []string
	for rest := plist; ; {
		open := strings.Index(rest, "<string>")
		if open < 0 {
			return out
		}
		rest = rest[open+len("<string>"):]
		end := strings.Index(rest, "</string>")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+len("</string>"):]
	}
}
