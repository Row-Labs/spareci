package gate

import (
	"os/exec"
	"time"
)

// SystemProbes reads power from pmset and idle time from the HID system's
// HIDIdleTime, which counts keyboard, mouse and trackpad input from any
// user. Both work from a background service too.
func SystemProbes() Probes {
	return Probes{
		OnAC: func() (bool, error) {
			out, err := exec.Command("pmset", "-g", "batt").Output()
			if err != nil {
				return false, err
			}
			return parsePmsetBatt(string(out))
		},
		Idle: func() (time.Duration, error) {
			out, err := exec.Command("ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
			if err != nil {
				return 0, err
			}
			return parseHIDIdleTime(string(out))
		},
		Now: time.Now,
	}
}
