// Package service installs `spareci up` as a per-user background service: a
// launchd agent on macOS, a systemd user unit on Linux. Either one starts it
// at login and restarts it if it crashes or is killed.
package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Spec is one service: `spareci up` for one repo.
type Spec struct {
	Repo string            // owner/name
	Exec string            // absolute path to the spareci binary
	Args []string          // arguments after the binary, starting with "up"
	Env  map[string]string // e.g. PATH, so `gh` is found
	Log  string            // stdout and stderr are appended here
}

// Name is the service's name for a repo, e.g. "spareci-row-labs-spareci".
func Name(repo string) string {
	var b strings.Builder
	b.WriteString("spareci-")
	for _, r := range strings.ToLower(repo) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default: // "/" and "."
			b.WriteRune('-')
		}
	}
	return b.String()
}

// LaunchdLabel is the launchd job label for a repo.
func LaunchdLabel(repo string) string {
	return "dev.spareci." + strings.TrimPrefix(Name(repo), "spareci-")
}

// Plist renders the launchd agent. KeepAlive with SuccessfulExit=false
// restarts spareci when it exits with an error or is killed by a signal (a
// crash, kill -9), and leaves it stopped after a clean shutdown.
func Plist(s Spec) string {
	var b bytes.Buffer
	x := func(v string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(v))
		return e.String()
	}
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
`, x(LaunchdLabel(s.Repo)))
	for _, a := range append([]string{s.Exec}, s.Args...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", x(a))
	}
	b.WriteString("\t</array>\n")
	if len(s.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedKeys(s.Env) {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", x(k), x(s.Env[k]))
		}
		b.WriteString("\t</dict>\n")
	}
	fmt.Fprintf(&b, `	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ExitTimeOut</key>
	<integer>60</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, x(s.Log), x(s.Log))
	return b.String()
}

// Unit renders the systemd user unit. Restart=on-failure restarts spareci
// after an error exit or an unclean signal such as SIGKILL; a clean stop
// (SIGTERM, which spareci treats like ^C) leaves it stopped.
func Unit(s Spec) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `[Unit]
Description=spareci: GitHub Actions jobs for %s on this machine
Documentation=https://github.com/Row-Labs/spareci

[Service]
Type=simple
ExecStart=%s
`, s.Repo, systemdArgs(append([]string{s.Exec}, s.Args...)))
	for _, k := range sortedKeys(s.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(k+"="+s.Env[k]))
	}
	fmt.Fprintf(&b, `Restart=on-failure
RestartSec=10
TimeoutStopSec=60
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, systemdEscape(s.Log), systemdEscape(s.Log))
	return b.String()
}

func systemdArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = systemdQuote(a)
	}
	return strings.Join(q, " ")
}

// systemdQuote quotes one word for ExecStart= or Environment=: specifiers (%)
// and variable expansion ($) are escaped as well as quotes and backslashes.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(systemdEscape(s)) + `"`
}

func systemdEscape(s string) string {
	return strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Path is where the service file for repo lives.
func Path(repo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel(repo)+".plist"), nil
	case "linux":
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "systemd", "user", Name(repo)+".service"), nil
	}
	return "", fmt.Errorf("background service not supported on %s", runtime.GOOS)
}

// Install writes the service file and starts the service, replacing any
// earlier install for the same repo.
func Install(s Spec) (string, error) {
	path, err := Path(s.Repo)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	content := Unit(s)
	if runtime.GOOS == "darwin" {
		content = Plist(s)
		unloadLaunchd(s.Repo)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return path, launchctl("bootstrap", launchdDomain(), path)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return path, err
	}
	if err := systemctl("enable", Name(s.Repo)+".service"); err != nil {
		return path, err
	}
	return path, systemctl("restart", Name(s.Repo)+".service")
}

// Uninstall stops the service (spareci gets SIGTERM and shuts down as on ^C)
// and removes its file. It reports whether anything was installed.
func Uninstall(repo string) (bool, error) {
	path, err := Path(repo)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	if runtime.GOOS == "darwin" {
		unloadLaunchd(repo)
	} else {
		_ = systemctl("disable", "--now", Name(repo)+".service")
	}
	if err := os.Remove(path); err != nil {
		return true, err
	}
	if runtime.GOOS == "linux" {
		return true, systemctl("daemon-reload")
	}
	return true, nil
}

// Installed lists the repos with an installed service file. The repo is read
// back from the file's --repo argument.
func Installed() ([]string, error) {
	path, err := Path("x/y")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "dev.spareci.") && !strings.HasPrefix(n, "spareci-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if r := repoArg(string(data)); r != "" {
			repos = append(repos, r)
		}
	}
	return repos, nil
}

// repoArg finds the value after --repo in a plist or unit file.
func repoArg(file string) string {
	f := strings.FieldsFunc(file, func(r rune) bool {
		return r == '<' || r == '>' || r == '"' || r == ' ' || r == '\n' || r == '\t'
	})
	for i, w := range f {
		if w == "--repo" {
			for _, v := range f[i+1:] {
				if v != "/string" && v != "string" {
					return v
				}
			}
		}
	}
	return ""
}

// unloadLaunchd stops and unloads the agent if it's loaded. bootout returns
// before the job is gone, and bootstrapping it again meanwhile fails with
// "Input/output error", so wait until launchd no longer knows it (spareci
// gets up to ExitTimeOut to shut down cleanly).
func unloadLaunchd(repo string) {
	if launchctl("bootout", launchdTarget(repo)) != nil {
		return // not loaded
	}
	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) && launchctl("print", launchdTarget(repo)) == nil {
		time.Sleep(500 * time.Millisecond)
	}
}

func launchdDomain() string            { return fmt.Sprintf("gui/%d", os.Getuid()) }
func launchdTarget(repo string) string { return launchdDomain() + "/" + LaunchdLabel(repo) }

func launchctl(args ...string) error { return run("launchctl", args...) }
func systemctl(args ...string) error { return run("systemctl", append([]string{"--user"}, args...)...) }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
