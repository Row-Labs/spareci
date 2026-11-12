package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// start runs a fake daemon on one socket and the proxy in front of it on
// another, and returns the proxy's socket.
func start(t *testing.T, daemon http.Handler, maps ...Map) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dp") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dl, err := net.Listen("unix", filepath.Join(dir, "d.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: daemon}
	go srv.Serve(dl)
	t.Cleanup(func() { srv.Close() })

	pl, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Upstream: filepath.Join(dir, "d.sock"), Maps: maps}
	go p.Serve(pl)
	t.Cleanup(func() { pl.Close() })
	return filepath.Join(dir, "p.sock")
}

func client(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

var maps = []Map{
	{"/home/runner", "/var/lib/docker/volumes/spareci-job-r1/_data"},
	{"/opt/hostedtoolcache", "/var/lib/docker/volumes/spareci-toolcache-o_r-arm64/_data"},
}

func TestRewritesContainerCreate(t *testing.T) {
	got := make(chan map[string]any, 1)
	sock := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var spec map[string]any
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		if err := d.Decode(&spec); err != nil {
			t.Error(err)
		}
		got <- spec
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"Id":"abc"}`)
	}), maps...)

	body := `{"Image":"alpine","HostConfig":{"Memory":9007199254740993,
		"Binds":["/home/runner/actions-runner/_work:/__w","/opt/hostedtoolcache:/__t:ro","named-vol:/data","/var/run/docker.sock:/var/run/docker.sock","/home/runnerX:/x","/home/runner/../etc:/e"],
		"Mounts":[{"Type":"bind","Source":"/home/runner","Target":"/h"},{"Type":"volume","Source":"/home/runner","Target":"/v"}]}}`
	resp, err := client(sock).Post("http://docker/v1.47/containers/create?name=x", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(b) != `{"Id":"abc"}` {
		t.Fatalf("response %d %s", resp.StatusCode, b)
	}
	spec := <-got
	hc := spec["HostConfig"].(map[string]any)
	wantBinds := []string{
		"/var/lib/docker/volumes/spareci-job-r1/_data/actions-runner/_work:/__w",
		"/var/lib/docker/volumes/spareci-toolcache-o_r-arm64/_data:/__t:ro",
		"named-vol:/data",
		"/var/run/docker.sock:/var/run/docker.sock",
		"/home/runnerX:/x",
		"/home/runner/../etc:/e", // not under /home/runner: left alone
	}
	for i, w := range wantBinds {
		if hc["Binds"].([]any)[i] != w {
			t.Errorf("bind %d = %v, want %s", i, hc["Binds"].([]any)[i], w)
		}
	}
	mounts := hc["Mounts"].([]any)
	if s := mounts[0].(map[string]any)["Source"]; s != "/var/lib/docker/volumes/spareci-job-r1/_data" {
		t.Errorf("bind mount source = %v", s)
	}
	if s := mounts[1].(map[string]any)["Source"]; s != "/home/runner" {
		t.Errorf("volume mount source changed: %v", s)
	}
	if m := hc["Memory"].(json.Number); m.String() != "9007199254740993" {
		t.Errorf("Memory = %s: large integers must survive", m)
	}
	if spec["Image"] != "alpine" {
		t.Errorf("other fields lost: %v", spec)
	}
}

func TestRewritesBindVolume(t *testing.T) {
	got := make(chan string, 1)
	sock := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		fmt.Fprint(w, `{}`)
	}), maps...)
	resp, err := client(sock).Post("http://docker/volumes/create", "application/json",
		strings.NewReader(`{"Name":"v","DriverOpts":{"type":"none","o":"bind","device":"/home/runner/data"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if b := <-got; !strings.Contains(b, `"device":"/var/lib/docker/volumes/spareci-job-r1/_data/data"`) {
		t.Errorf("volume spec = %s", b)
	}
}

// Requests that don't create mounts reach the daemon as sent, several over
// one kept-alive connection, and a body that isn't JSON is passed through.
func TestPassesThrough(t *testing.T) {
	sock := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.RequestURI(), b)
	}), maps...)
	c := client(sock)
	for _, tc := range []struct{ method, url, body, want string }{
		{"GET", "/v1.47/containers/json?all=1&filters=%7B%22label%22%3A%5B%22a%3Db%22%5D%7D", "", `GET /v1.47/containers/json?all=1&filters=%7B%22label%22%3A%5B%22a%3Db%22%5D%7D `},
		{"POST", "/build?t=x", "/home/runner tar bytes", "POST /build?t=x /home/runner tar bytes"},
		{"POST", "/containers/create", "not json /home/runner", "POST /containers/create not json /home/runner"},
		{"GET", "/_ping", "", "GET /_ping "},
	} {
		req, _ := http.NewRequest(tc.method, "http://docker"+tc.url, strings.NewReader(tc.body))
		if tc.body == "" {
			req.Body = nil
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != tc.want {
			t.Errorf("%s %s: daemon saw %q, want %q", tc.method, tc.url, b, tc.want)
		}
	}
}

// Streamed responses (logs, events, pull progress) arrive as the daemon
// flushes them, not when it finishes.
func TestStreamsResponses(t *testing.T) {
	release := make(chan struct{})
	sock := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "first")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprintln(w, "second")
	}), maps...)
	defer close(release)
	resp, err := client(sock).Get("http://docker/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "first\n" {
			t.Errorf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streamed line not delivered")
	}
}

// An upgraded connection (docker attach, exec) is a raw stream both ways, and
// the client's half-close reaches the daemon (stdin EOF), which can still
// answer after it.
func TestUpgradeHalfClose(t *testing.T) {
	sock := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		in, _ := io.ReadAll(brw) // until the client's half-close
		fmt.Fprintf(conn, "echo:%s:eof", in)
	}), maps...)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST /v1.47/containers/abc/attach?stream=1&stdin=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	fmt.Fprint(conn, "stdin data")
	conn.(*net.UnixConn).CloseWrite()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(all), "HTTP/1.1 101") || !strings.HasSuffix(string(all), "\r\n\r\necho:stdin data:eof") {
		t.Errorf("got %q", all)
	}
}

func TestDaemonDown(t *testing.T) {
	dir, _ := os.MkdirTemp("", "dp")
	defer os.RemoveAll(dir)
	pl, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	go (&Proxy{Upstream: filepath.Join(dir, "nope.sock")}).Serve(pl)
	resp, err := client(filepath.Join(dir, "p.sock")).Get("http://docker/_ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestParseMaps(t *testing.T) {
	m, err := parseMaps([]string{"/home/runner/=/var/lib/docker/volumes/v/_data"})
	if err != nil || len(m) != 1 || m[0] != (Map{"/home/runner", "/var/lib/docker/volumes/v/_data"}) {
		t.Errorf("parseMaps = %v, %v", m, err)
	}
	for _, bad := range []string{"relative=/x", "/x", "/x=y"} {
		if _, err := parseMaps([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMapPathLongestPrefix(t *testing.T) {
	p := &Proxy{Maps: []Map{{"/home/runner", "/a"}, {"/home/runner/cache", "/b"}}}
	for in, want := range map[string]string{
		"/home/runner/x":       "/a/x",
		"/home/runner/cache/y": "/b/y",
		"/home/runner":         "/a",
	} {
		if got, ok := p.mapPath(in); !ok || got != want {
			t.Errorf("mapPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
