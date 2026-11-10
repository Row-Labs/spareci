package main

import "os"

// isTerminal reports whether f is an interactive terminal. /dev/null is a
// character device too, so it is ruled out explicitly.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}
