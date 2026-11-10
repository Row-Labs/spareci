package gate

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The parsers below are OS-independent so they can be tested anywhere; the
// probes_*.go files run the commands and read the files.

// parsePmsetBatt reads the first line of `pmset -g batt`:
//
//	Now drawing from 'AC Power'
//	Now drawing from 'Battery Power'
//
// A UPS counts as battery: the machine is no longer on mains.
func parsePmsetBatt(out string) (bool, error) {
	line, _, _ := strings.Cut(out, "\n")
	_, src, ok := strings.Cut(line, "drawing from '")
	if !ok {
		return false, fmt.Errorf("unexpected pmset output: %q", line)
	}
	src, _, _ = strings.Cut(src, "'")
	return src == "AC Power", nil
}

var hidIdleRe = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// parseHIDIdleTime reads HIDIdleTime (nanoseconds) from `ioreg -c IOHIDSystem`.
func parseHIDIdleTime(out string) (time.Duration, error) {
	m := hidIdleRe.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("HIDIdleTime not found in ioreg output")
	}
	ns, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}

// linuxOnAC reads /sys/class/power_supply (root is that directory). Any
// online "Mains" or "USB" supply means AC. With none online, a battery that
// is discharging means battery power. A machine with no battery at all is a
// desktop and counts as AC.
func linuxOnAC(root string) (bool, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	read := func(dir, name string) string {
		b, _ := os.ReadFile(filepath.Join(root, dir, name))
		return strings.TrimSpace(string(b))
	}
	mains, batteries, discharging := 0, 0, 0
	for _, e := range ents {
		switch read(e.Name(), "type") {
		case "Mains", "USB":
			mains++
			if read(e.Name(), "online") == "1" {
				return true, nil
			}
		case "Battery":
			if read(e.Name(), "scope") == "Device" {
				continue // a mouse or keyboard battery, not ours
			}
			batteries++
			if read(e.Name(), "status") == "Discharging" {
				discharging++
			}
		}
	}
	if batteries == 0 {
		return true, nil
	}
	if mains > 0 {
		return false, nil // adapters present, none online
	}
	return discharging == 0, nil
}

var mutterIdleRe = regexp.MustCompile(`uint64 (\d+)`)

// parseMutterIdle reads GNOME's IdleMonitor.GetIdletime reply, "(uint64 1234,)", in ms.
func parseMutterIdle(out string) (time.Duration, error) {
	m := mutterIdleRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("unexpected IdleMonitor reply: %q", strings.TrimSpace(out))
	}
	ms, err := strconv.ParseInt(m[1], 10, 64)
	return time.Duration(ms) * time.Millisecond, err
}

// parseMillis reads xprintidle's output: idle time in ms.
func parseMillis(out string) (time.Duration, error) {
	ms, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected xprintidle output: %q", strings.TrimSpace(out))
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// parseLogindIdle reads `loginctl show-session -p IdleHint -p IdleSinceHint`.
// IdleSinceHint is microseconds since the epoch. logind only knows a session
// is idle once the desktop (or a screen locker) tells it, so this is coarse.
func parseLogindIdle(out string, now time.Time) (time.Duration, error) {
	var hint string
	var since int64 = -1
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "IdleHint":
			hint = v
		case "IdleSinceHint":
			since, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	switch {
	case hint == "no":
		return 0, nil
	case hint == "yes" && since > 0:
		return max(now.Sub(time.UnixMicro(since)), 0), nil
	}
	return 0, fmt.Errorf("unexpected loginctl output: %q", strings.TrimSpace(out))
}
