package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/Row-Labs/spareci/internal/route"
)

func TestOSFlag(t *testing.T) {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	c := commonFlags(fs)
	if err := fs.Parse([]string{"--os", "macOS", "--os", "linux,macos"}); err != nil {
		t.Fatal(err)
	}
	if got := c.oses.String(); got != "macos,linux" {
		t.Errorf("--os = %q", got)
	}
	if !c.split() || !c.serves(route.Linux) {
		t.Error("not split")
	}
	if err := fs.Parse([]string{"--os", "windows"}); err == nil {
		t.Error("--os windows accepted")
	}

	c = commonFlags(flag.NewFlagSet("up", flag.ContinueOnError))
	if c.split() || !c.serves(route.MacOS) || !c.serves(route.Linux) || len(c.targets()) != len(route.Targets) {
		t.Error("without --os a machine serves every OS")
	}
	r := c.router(nil, []route.Target{route.TargetMacOS})
	if len(r.Groups) != 2 || !r.Groups[1].Legacy() || r.OldHeartbeat != "" {
		t.Errorf("single machine router: %+v", r.Groups)
	}
	c.oses = osFlags{route.Linux}
	r = c.router(nil, c.targets())
	if len(r.Groups) != 1 || r.Groups[0].Heartbeat != "SPARECI_HEARTBEAT_LINUX" || r.OldHeartbeat != "SPARECI_HEARTBEAT" {
		t.Errorf("--os linux router: %+v", r)
	}
}

func TestConflictHint(t *testing.T) {
	c := &common{oses: osFlags{route.MacOS, route.Linux}}
	owner := route.Owner{Group: route.Group{OS: route.Linux, Heartbeat: "SPARECI_HEARTBEAT_LINUX"}, Machine: "server", Age: 20 * time.Second, Beating: true}
	err := conflictHint(&route.ConflictError{Repo: "o/r", Owner: owner}, c)
	var ce *route.ConflictError
	if !errors.As(err, &ce) {
		t.Fatal("hint lost the conflict")
	}
	for _, want := range []string{"server is serving Linux jobs for o/r (heartbeat 20s ago", "--os macos", "spareci down --name server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing %q", err, want)
		}
	}
	if err := conflictHint(errors.New("other"), c); err.Error() != "other" {
		t.Error(err)
	}
}
