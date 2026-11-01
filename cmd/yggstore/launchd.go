package main

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// On a Mac, join -service starts the node and dashboard at login as
// launchd agents (~/Library/LaunchAgents), as systemd user units do on
// Linux. A unit name such as yggstore-node.service becomes the agent
// org.yggstore.yggstore-node.

func agentLabel(unit string) string { return "org.yggstore." + strings.TrimSuffix(unit, ".service") }

func agentPath(unit string) string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", agentLabel(unit)+".plist")
}

// agentsFree checks that none of the agents exists yet.
func agentsFree(units ...string) error {
	for _, u := range units {
		if _, err := os.Stat(agentPath(u)); err == nil {
			return fmt.Errorf("%s already exists, so a node is set up here already; join with its own -dir, -port and -name, or without -service", agentPath(u))
		}
	}
	return nil
}

func agent(label string, cmd []string) string {
	var args strings.Builder
	for _, a := range cmd {
		args.WriteString("\t\t<string>" + html.EscapeString(a) + "</string>\n")
	}
	logs := filepath.Join(homeDir(), "Library", "Logs", label+".log")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>%s</string>
	</dict>
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
`, html.EscapeString(label), args.String(), html.EscapeString(homeDir()), html.EscapeString(logs), html.EscapeString(logs))
}

// installAgents writes the agents and starts them.
func installAgents(agents map[string][]string) error {
	dir := filepath.Join(homeDir(), "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(homeDir(), "Library", "Logs"), 0o755)
	for unit, cmd := range agents {
		path := agentPath(unit)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; not overwriting it", path)
		}
		if err := os.WriteFile(path, []byte(agent(agentLabel(unit), cmd)), 0o644); err != nil {
			return err
		}
		if out, err := exec.Command("launchctl", "load", "-w", path).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl load %s: %s", path, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
