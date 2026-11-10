package cache

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultUpstream is GitHub's results service, which also serves artifacts.
// Calls we don't handle are passed through to it.
const DefaultUpstream = "https://results-receiver.actions.githubusercontent.com/"

const twirpCache = "/twirp/github.actions.results.api.v1.CacheService/"

// Server hands each job its own listener bound to that job's repo, so a job
// can only reach its repo's caches and loses access when it ends.
type Server struct {
	store    *Store
	upstream *url.URL
	bindHost string // address jobs reach us on; 127.0.0.1 while jobs run on the host
	secret   []byte // signs upload/download URLs
}

func NewServer(store *Store, upstream string) (*Server, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &Server{store: store, upstream: u, bindHost: "127.0.0.1", secret: secret}, nil
}

func (s *Server) Store() *Store { return s.store }

// JobListener serves one job.
type JobListener struct {
	URL  string // value for ACTIONS_RESULTS_URL
	srv  *http.Server
	repo string
}

// Env is what the job's runner process needs to use this listener.
//
// SPARECI_RESULTS_URL and SPARECI_BUILDKIT_RESULTS_URL are for spareci's
// docker shim (internal/runner/docker.go): BuildKit runs in a container and
// can't reach 127.0.0.1. On macOS the Docker VM forwards
// host.docker.internal to the Mac's loopback (checked with OrbStack; Docker
// Desktop documents the same), so BuildKit gets this listener under that
// name. Elsewhere the shim falls back to GitHub's cache service.
func (j *JobListener) Env() []string {
	env := []string{"ACTIONS_RESULTS_URL=" + j.URL, "ACTIONS_CACHE_SERVICE_V2=true", "SPARECI_RESULTS_URL=" + j.URL}
	if runtime.GOOS == "darwin" {
		env = append(env, "SPARECI_BUILDKIT_RESULTS_URL="+strings.Replace(j.URL, "//127.0.0.1:", "//host.docker.internal:", 1))
	}
	return env
}

func (j *JobListener) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = j.srv.Shutdown(ctx)
}

// Register opens a listener for one job in repo.
func (s *Server) Register(repo string) (*JobListener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.bindHost, "0"))
	if err != nil {
		return nil, err
	}
	base := "http://" + ln.Addr().String()
	h := &jobHandler{s: s, repo: repo, base: base}
	proxy := httputil.NewSingleHostReverseProxy(s.upstream)
	proxy.Director = func(r *http.Request) {
		r.URL.Scheme = s.upstream.Scheme
		r.URL.Host = s.upstream.Host
		r.Host = s.upstream.Host
	}
	h.proxy = proxy
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return &JobListener{URL: base + "/", srv: srv, repo: repo}, nil
}

type jobHandler struct {
	s     *Server
	repo  string
	base  string
	proxy *httputil.ReverseProxy
}

func (h *jobHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(p, twirpCache):
		h.twirp(w, r, strings.TrimPrefix(p, twirpCache))
	case strings.HasPrefix(p, "/twirp/"):
		h.proxy.ServeHTTP(w, r) // artifacts and anything else: GitHub's own service
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/u/"):
		h.upload(w, r)
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasPrefix(p, "/d/"):
		h.download(w, r)
	default:
		http.NotFound(w, r)
	}
}

// --- Twirp (JSON) --------------------------------------------------------
//
// actions/cache sends protobuf JSON with lowerCamelCase names; we also accept
// the proto field names, which protobuf JSON parsers must. Responses carry
// both spellings: @actions/cache reads lowerCamelCase, while BuildKit's
// type=gha client (tonistiigi/go-actions-cache) decodes with encoding/json
// and reads only the proto names (signed_upload_url, ...).

type twirpReq struct {
	Key          string          `json:"key"`
	Version      string          `json:"version"`
	RestoreKeys  []string        `json:"restoreKeys"`
	RestoreKeys2 []string        `json:"restore_keys"`
	SizeBytes    json.RawMessage `json:"sizeBytes"`
}

func (h *jobHandler) twirp(w http.ResponseWriter, r *http.Request, method string) {
	var req twirpReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	if req.Key == "" || req.Version == "" {
		twirpError(w, http.StatusBadRequest, "invalid_argument", "key and version are required")
		return
	}
	scopes := ScopesFromAuth(r.Header.Get("Authorization"))

	switch method {
	case "GetCacheEntryDownloadURL":
		read := make([]string, 0, len(scopes))
		for _, sc := range scopes {
			if sc.Read() {
				read = append(read, sc.Scope)
			}
		}
		e := h.s.store.Lookup(h.repo, read, req.Key, append(req.RestoreKeys, req.RestoreKeys2...), req.Version)
		if e == nil {
			slog.Info("cache miss", "repo", h.repo, "key", req.Key)
			writeJSON(w, map[string]any{"ok": false, "signedDownloadUrl": "", "matchedKey": "", "signed_download_url": "", "matched_key": ""})
			return
		}
		slog.Info("cache hit", "repo", h.repo, "key", e.Key, "size", HumanBytes(e.Size))
		u := h.sign(r, "d", strconv.FormatInt(e.ID, 10))
		writeJSON(w, map[string]any{"ok": true, "signedDownloadUrl": u, "matchedKey": e.Key, "signed_download_url": u, "matched_key": e.Key})

	case "CreateCacheEntry":
		scope, ok := writeScope(scopes)
		if !ok {
			writeJSON(w, map[string]any{"ok": false, "signedUploadUrl": "", "signed_upload_url": "", "message": "job token cannot write to the cache"})
			return
		}
		id, err := h.s.store.Reserve(h.repo, scope, req.Key, req.Version)
		if errors.Is(err, ErrExists) {
			writeJSON(w, map[string]any{"ok": false, "signedUploadUrl": "", "signed_upload_url": ""})
			return
		}
		if err != nil {
			twirpError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		u := h.sign(r, "u", id)
		writeJSON(w, map[string]any{"ok": true, "signedUploadUrl": u, "signed_upload_url": u})

	case "FinalizeCacheEntryUpload":
		scope, _ := writeScope(scopes)
		e, err := h.s.store.Finalize(h.repo, scope, req.Key, req.Version)
		if err != nil {
			slog.Warn("cache save failed", "repo", h.repo, "key", req.Key, "err", err)
			writeJSON(w, map[string]any{"ok": false, "entryId": "0", "entry_id": "0"})
			return
		}
		slog.Info("cache saved", "repo", h.repo, "key", e.Key, "scope", e.Scope, "size", HumanBytes(e.Size))
		writeJSON(w, map[string]any{"ok": true, "entryId": strconv.FormatInt(e.ID, 10), "entry_id": strconv.FormatInt(e.ID, 10)})

	default:
		twirpError(w, http.StatusNotFound, "bad_route", "unsupported method "+method)
	}
}

func writeScope(scopes []Scope) (string, bool) {
	for _, sc := range scopes {
		if sc.Write() {
			return sc.Scope, true
		}
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func twirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": msg})
}

// --- Signed blob URLs ----------------------------------------------------
//
// Paths have three segments (/kind/signature/id) because the Azure Blob SDK
// that actions/cache uploads with parses IP-host URLs as
// /account/container/blob.

func (h *jobHandler) sig(kind, id string) string {
	m := hmac.New(sha256.New, h.s.secret)
	fmt.Fprintf(m, "%s\x00%s\x00%s", kind, h.repo, id)
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// sign returns a signed blob URL on the host the client reached us by. Most
// clients use the 127.0.0.1 URL from ACTIONS_RESULTS_URL, but BuildKit runs
// in a container and reaches this listener as host.docker.internal (see
// buildkitURL), and must be sent back there for the blob too.
func (h *jobHandler) sign(r *http.Request, kind, id string) string {
	base := h.base
	if r.Host != "" {
		base = "http://" + r.Host
	}
	return fmt.Sprintf("%s/%s/%s/%s", base, kind, h.sig(kind, id), id)
}

func (h *jobHandler) verify(r *http.Request, kind string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != kind {
		return "", false
	}
	return parts[2], hmac.Equal([]byte(parts[1]), []byte(h.sig(kind, parts[2])))
}

// upload speaks the slice of the Azure Blob REST API the SDK uses:
// a single-shot Put Blob, or Put Block then Put Block List.
func (h *jobHandler) upload(w http.ResponseWriter, r *http.Request) {
	id, ok := h.verify(r, "u")
	if !ok {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	var err error
	switch q.Get("comp") {
	case "":
		_, err = h.s.store.PutBlob(h.repo, id, r.Body)
	case "block":
		_, err = h.s.store.PutBlock(h.repo, id, q.Get("blockid"), r.Body)
	case "blocklist":
		var list blockList
		if err = xml.NewDecoder(r.Body).Decode(&list); err == nil {
			err = h.s.store.CommitBlocks(h.repo, id, list.IDs())
		}
	default:
		err = fmt.Errorf("unsupported comp=%s", q.Get("comp"))
	}
	if err != nil {
		slog.Warn("cache upload failed", "repo", h.repo, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	w.Header().Set("ETag", fmt.Sprintf("\"0x%X\"", now.UnixNano()))
	w.Header().Set("Last-Modified", now.Format(http.TimeFormat))
	w.Header().Set("x-ms-request-id", id)
	w.Header().Set("x-ms-version", "2024-08-04")
	w.Header().Set("x-ms-request-server-encrypted", "true")
	w.Header().Set("Date", now.Format(http.TimeFormat))
	if md5 := r.Header.Get("Content-MD5"); md5 != "" {
		w.Header().Set("Content-MD5", md5)
	}
	w.WriteHeader(http.StatusCreated)
}

type blockList struct {
	Items []struct {
		XMLName xml.Name
		ID      string `xml:",chardata"`
	} `xml:",any"`
}

func (b blockList) IDs() []string {
	ids := make([]string, 0, len(b.Items))
	for _, it := range b.Items { // <Latest>, <Committed> or <Uncommitted>, in order
		ids = append(ids, strings.TrimSpace(it.ID))
	}
	return ids
}

func (h *jobHandler) download(w http.ResponseWriter, r *http.Request) {
	idStr, ok := h.verify(r, "d")
	if !ok {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := h.s.store.OpenBlob(h.repo, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", fi.ModTime(), f) // handles Range for parallel downloads
}

// --- Job token scopes ----------------------------------------------------

// Scope is one git ref a job's token may read (1) and/or write (2) caches for.
// GitHub encodes these in the "ac" claim of ACTIONS_RUNTIME_TOKEN: a push to
// main gets refs/heads/main read+write; a pull request gets its merge ref
// read+write and its base branch read-only.
type Scope struct {
	Scope      string `json:"Scope"`
	Permission int    `json:"Permission"`
}

func (s Scope) Read() bool  { return s.Permission&1 != 0 }
func (s Scope) Write() bool { return s.Permission&2 != 0 }

// ScopesFromAuth reads scopes from a "Bearer <jwt>" header, writable ones
// first. The token is not verified: the listener is already bound to one
// job, and this only decides which of that repo's refs it sees. Without a
// readable claim, everything shares one scope.
func ScopesFromAuth(header string) []Scope {
	fallback := []Scope{{Scope: "", Permission: 3}}
	tok := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return fallback
	}
	var claims struct {
		AC json.RawMessage `json:"ac"`
	}
	if json.Unmarshal(payload, &claims) != nil || len(claims.AC) == 0 {
		return fallback
	}
	raw := claims.AC
	var asString string
	if json.Unmarshal(raw, &asString) == nil { // usually a JSON string holding JSON
		raw = []byte(asString)
	}
	var scopes []Scope
	if json.Unmarshal(raw, &scopes) != nil || len(scopes) == 0 {
		return fallback
	}
	out := make([]Scope, 0, len(scopes))
	for _, s := range scopes {
		if s.Write() {
			out = append(out, s)
		}
	}
	for _, s := range scopes {
		if !s.Write() {
			out = append(out, s)
		}
	}
	return out
}
