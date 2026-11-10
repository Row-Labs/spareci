package gate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// SystemProbes reads power from /sys/class/power_supply. Idle time on Linux
// is best effort, tried in this order:
//
//  1. GNOME's IdleMonitor over the session D-Bus (gdbus), which works on
//     Wayland and X11.
//  2. xprintidle, on X11 with $DISPLAY set.
//  3. systemd-logind's IdleHint for $XDG_SESSION_ID, which only turns idle
//     when the desktop or screen locker reports it.
//
// Run from a system service without the user's session bus, none of these
// may work, and `spareci up --idle` says so at start.
func SystemProbes() Probes {
	return Probes{
		OnAC: func() (bool, error) { return linuxOnAC("/sys/class/power_supply") },
		Idle: linuxIdle,
		Now:  time.Now,
	}
}

func linuxIdle() (time.Duration, error) {
	var errs []error
	if out, err := exec.Command("gdbus", "call", "--session",
		"--dest", "org.gnome.Mutter.IdleMonitor",
		"--object-path", "/org/gnome/Mutter/IdleMonitor/Core",
		"--method", "org.gnome.Mutter.IdleMonitor.GetIdletime").Output(); err == nil {
		return parseMutterIdle(string(out))
	} else {
		errs = append(errs, fmt.Errorf("gnome idle monitor: %w", err))
	}
	if os.Getenv("DISPLAY") != "" {
		if out, err := exec.Command("xprintidle").Output(); err == nil {
			return parseMillis(string(out))
		} else {
			errs = append(errs, fmt.Errorf("xprintidle: %w", err))
		}
	}
	if id := os.Getenv("XDG_SESSION_ID"); id != "" {
		if out, err := exec.Command("loginctl", "show-session", id, "-p", "IdleHint", "-p", "IdleSinceHint").Output(); err == nil {
			return parseLogindIdle(string(out), time.Now())
		} else {
			errs = append(errs, fmt.Errorf("loginctl: %w", err))
		}
	}
	return 0, fmt.Errorf("no idle source found (tried GNOME, xprintidle, logind): %w", errors.Join(errs...))
}
