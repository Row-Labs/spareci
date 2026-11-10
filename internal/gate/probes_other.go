//go:build !darwin && !linux

package gate

import "time"

// SystemProbes has no power or idle source on this OS; --only-on-ac and
// --idle are refused at start. --hours still works.
func SystemProbes() Probes { return Probes{Now: time.Now} }
