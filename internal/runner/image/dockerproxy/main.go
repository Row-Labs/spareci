// Command spareci-dockerproxy sits between a job and the Docker daemon in
// spareci's Linux job containers (internal/runner/image). It passes the Docker
// API through unchanged, except that bind-mount sources under the job's own
// paths are rewritten to where the daemon sees them.
//
// Why: a job's home and workspace live in a per-job volume, mounted at the
// same path (/home/runner) in every job, so paths a job sees, and caches keyed
// on them, are the same for every job. The daemon resolves bind mounts on its
// own host, where that volume is at <DockerRootDir>/volumes/<name>/_data. So
// `docker run -v /home/runner/work/x:/x`, from the runner (container: jobs,
// services:, Docker actions), a step, docker compose or any SDK, has
// /home/runner rewritten to the volume's path.
//
// Usage: spareci-dockerproxy -listen PATH -upstream PATH [FROM=TO ...]
//
// It is built into the job image and only uses the standard library, so it
// builds from this one file (the image build has no go.mod).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
)

func main() {
	listen := flag.String("listen", "/var/run/docker.sock", "socket to listen on")
	upstream := flag.String("upstream", "", "the daemon's socket")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("spareci-dockerproxy: ")
	maps, err := parseMaps(flag.Args())
	if err != nil || *upstream == "" {
		log.Fatalf("usage: spareci-dockerproxy -listen PATH -upstream PATH [FROM=TO ...]: %v", err)
	}
	_ = os.Remove(*listen)
	l, err := net.Listen("unix", *listen)
	if err != nil {
		log.Fatal(err)
	}
	p := &Proxy{Upstream: *upstream, Maps: maps}
	log.Fatal(p.Serve(l))
}

// Map rewrites paths equal to or under From to the same place under To.
type Map struct{ From, To string }

func parseMaps(args []string) ([]Map, error) {
	var maps []Map
	for _, a := range args {
		from, to, ok := strings.Cut(a, "=")
		if !ok || !strings.HasPrefix(from, "/") || !strings.HasPrefix(to, "/") {
			return nil, fmt.Errorf("mapping %q: want /from=/to", a)
		}
		maps = append(maps, Map{strings.TrimRight(from, "/"), strings.TrimRight(to, "/")})
	}
	return maps, nil
}

// Proxy forwards connections to the daemon's socket.
type Proxy struct {
	Upstream string
	Maps     []Map
}

// Serve accepts connections until l is closed.
func (p *Proxy) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go p.handle(c)
	}
}

// handle relays one client connection over one daemon connection. Responses
// are copied back byte for byte; requests are parsed, so the ones that create
// mounts can be rewritten. After a request that upgrades the connection
// (attach, exec, BuildKit sessions), both directions are raw streams, and a
// half-close is passed on, so a client's stdin EOF reaches the container.
func (p *Proxy) handle(client net.Conn) {
	defer client.Close()
	up, err := net.Dial("unix", p.Upstream)
	if err != nil {
		log.Print(err)
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(err.Error()), err)
		return
	}
	defer up.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(client, up); err != nil {
			// The client is gone: stop reading its requests too.
			client.Close()
			return
		}
		closeWrite(client)
	}()

	br := bufio.NewReader(client)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			break
		}
		if err := p.rewrite(req); err != nil {
			log.Printf("%s %s: %v; passed through unchanged", req.Method, req.URL.Path, err)
		}
		if err := req.Write(up); err != nil {
			break
		}
		if req.Header.Get("Upgrade") != "" {
			_, _ = io.Copy(up, br)
			break
		}
	}
	closeWrite(up)
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// The API calls that take host paths, with or without a /v1.xx prefix.
var (
	containerCreate = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)
	volumeCreate    = regexp.MustCompile(`^(/v[0-9.]+)?/volumes/create$`)
)

// maxBody bounds the request bodies read to rewrite them (they're JSON specs).
const maxBody = 16 << 20

// rewrite rewrites the host paths in a container or volume spec. On an error
// the request is left as it was, body included.
func (p *Proxy) rewrite(req *http.Request) error {
	if req.Method != http.MethodPost || len(p.Maps) == 0 {
		return nil
	}
	var edit func(map[string]any) bool
	switch {
	case containerCreate.MatchString(req.URL.Path):
		edit = p.editContainer
	case volumeCreate.MatchString(req.URL.Path):
		edit = p.editVolume
	default:
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	req.Body.Close()
	setBody(req, body)
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return errors.New("body too large to rewrite")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep large integers (memory limits) exact
	var spec map[string]any
	if err := dec.Decode(&spec); err != nil {
		return err
	}
	if !edit(spec) {
		return nil
	}
	out, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	setBody(req, out)
	return nil
}

func setBody(req *http.Request, b []byte) {
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.ContentLength = int64(len(b))
	req.TransferEncoding = nil
}

// editContainer rewrites HostConfig.Binds ("src:dst[:opts]") and bind
// HostConfig.Mounts. Volume names (no leading /) are left alone.
func (p *Proxy) editContainer(spec map[string]any) bool {
	hc, _ := spec["HostConfig"].(map[string]any)
	if hc == nil {
		return false
	}
	changed := false
	if binds, ok := hc["Binds"].([]any); ok {
		for i, b := range binds {
			s, ok := b.(string)
			if !ok {
				continue
			}
			src, rest, _ := strings.Cut(s, ":")
			if to, ok := p.mapPath(src); ok {
				if rest != "" {
					to += ":" + rest
				}
				binds[i], changed = to, true
			}
		}
	}
	if mounts, ok := hc["Mounts"].([]any); ok {
		for _, m := range mounts {
			mm, ok := m.(map[string]any)
			if !ok || mm["Type"] != "bind" {
				continue
			}
			if src, ok := mm["Source"].(string); ok {
				if to, ok := p.mapPath(src); ok {
					mm["Source"], changed = to, true
				}
			}
		}
	}
	return changed
}

// editVolume rewrites the device of a local bind volume
// (docker volume create -o type=none -o o=bind -o device=PATH).
func (p *Proxy) editVolume(spec map[string]any) bool {
	opts, _ := spec["DriverOpts"].(map[string]any)
	dev, _ := opts["device"].(string)
	if to, ok := p.mapPath(dev); ok {
		opts["device"] = to
		return true
	}
	return false
}

// mapPath maps an absolute path under a mapping's From. The longest From
// wins. The path is cleaned first, so /home/runner/.. isn't under /home/runner.
func (p *Proxy) mapPath(s string) (string, bool) {
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	s = path.Clean(s)
	best := -1
	for i, m := range p.Maps {
		if (s == m.From || strings.HasPrefix(s, m.From+"/")) && (best < 0 || len(m.From) > len(p.Maps[best].From)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return p.Maps[best].To + strings.TrimPrefix(s, p.Maps[best].From), true
}
