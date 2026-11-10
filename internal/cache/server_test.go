package cache

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// token builds an unsigned JWT with GitHub's "ac" claim.
func token(scopes ...Scope) string {
	ac, _ := json.Marshal(scopes)
	claims, _ := json.Marshal(map[string]any{"ac": string(ac)})
	enc := base64.RawURLEncoding.EncodeToString
	return "Bearer " + enc([]byte(`{"alg":"none"}`)) + "." + enc(claims) + ".sig"
}

var mainToken = token(Scope{"refs/heads/main", 3})

func newServer(t *testing.T, upstream string) *Server {
	t.Helper()
	store, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if upstream == "" {
		upstream = DefaultUpstream
	}
	s, err := NewServer(store, upstream)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func listen(t *testing.T, s *Server, repo string) *JobListener {
	t.Helper()
	jl, err := s.Register(repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jl.Close)
	return jl
}

func call(t *testing.T, jl *JobListener, auth, method string, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", jl.URL+"twirp/github.actions.results.api.v1.CacheService/"+method, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

func put(t *testing.T, url string, body []byte, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(body))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("PUT %s: %d, want %d", url, resp.StatusCode, wantStatus)
	}
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// The same calls actions/cache makes: create, upload, finalize; then look up
// and download.
func TestSaveAndRestoreSingleShot(t *testing.T) {
	s := newServer(t, "")
	jl := listen(t, s, "o/r")

	miss := call(t, jl, mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v"})
	if miss["ok"] != false {
		t.Fatalf("expected miss: %v", miss)
	}

	created := call(t, jl, mainToken, "CreateCacheEntry", map[string]any{"key": "k-1", "version": "v"})
	if created["ok"] != true {
		t.Fatalf("create: %v", created)
	}
	put(t, created["signedUploadUrl"].(string), []byte("archive-bytes"), http.StatusCreated)
	fin := call(t, jl, mainToken, "FinalizeCacheEntryUpload", map[string]any{"key": "k-1", "version": "v", "sizeBytes": "13"})
	if fin["ok"] != true || fin["entryId"] == "0" {
		t.Fatalf("finalize: %v", fin)
	}

	dup := call(t, jl, mainToken, "CreateCacheEntry", map[string]any{"key": "k-1", "version": "v"})
	if dup["ok"] != false {
		t.Fatalf("second create of same key should be refused: %v", dup)
	}

	hit := call(t, jl, mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "k-2", "restoreKeys": []string{"k-"}, "version": "v"})
	if hit["ok"] != true || hit["matchedKey"] != "k-1" {
		t.Fatalf("restore: %v", hit)
	}
	status, body := get(t, hit["signedDownloadUrl"].(string))
	if status != 200 || string(body) != "archive-bytes" {
		t.Fatalf("download: %d %q", status, body)
	}
}

func TestBlockUpload(t *testing.T) {
	s := newServer(t, "")
	jl := listen(t, s, "o/r")
	up := call(t, jl, mainToken, "CreateCacheEntry", map[string]any{"key": "big", "version": "v"})["signedUploadUrl"].(string)
	ids := []string{"MDAwMDA=", "MDAwMDE="}
	put(t, up+"?comp=block&blockid="+ids[0], []byte("first-"), http.StatusCreated)
	put(t, up+"?comp=block&blockid="+ids[1], []byte("second"), http.StatusCreated)
	list := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>%s</Latest><Latest>%s</Latest></BlockList>`, ids[0], ids[1])
	put(t, up+"?comp=blocklist", []byte(list), http.StatusCreated)
	if fin := call(t, jl, mainToken, "FinalizeCacheEntryUpload", map[string]any{"key": "big", "version": "v"}); fin["ok"] != true {
		t.Fatalf("finalize: %v", fin)
	}
	hit := call(t, jl, mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "big", "version": "v"})
	if _, body := get(t, hit["signedDownloadUrl"].(string)); string(body) != "first-second" {
		t.Fatalf("got %q", body)
	}
}

func TestURLsAreSignedAndRepoBound(t *testing.T) {
	s := newServer(t, "")
	a := listen(t, s, "o/a")
	b := listen(t, s, "o/b")

	up := call(t, a, mainToken, "CreateCacheEntry", map[string]any{"key": "k", "version": "v"})["signedUploadUrl"].(string)
	put(t, strings.Replace(up, "/u/", "/u/0", 1), []byte("x"), http.StatusForbidden) // tampered signature
	put(t, up, []byte("secret"), http.StatusCreated)
	call(t, a, mainToken, "FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v"})
	down := call(t, a, mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v"})["signedDownloadUrl"].(string)

	// Repo b cannot see a's entry, nor use a's URL on its own listener.
	if hit := call(t, b, mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v"}); hit["ok"] != false {
		t.Fatalf("repo b saw repo a's cache: %v", hit)
	}
	if status, _ := get(t, strings.Replace(down, a.URL, b.URL, 1)); status != http.StatusForbidden {
		t.Fatalf("cross-repo download: %d", status)
	}
}

func TestReadOnlyScopeCannotSave(t *testing.T) {
	s := newServer(t, "")
	jl := listen(t, s, "o/r")
	ro := token(Scope{"refs/heads/main", 1})
	if out := call(t, jl, ro, "CreateCacheEntry", map[string]any{"key": "k", "version": "v"}); out["ok"] != false {
		t.Fatalf("read-only token saved: %v", out)
	}
}

func TestOtherCallsPassThrough(t *testing.T) {
	var gotPath, gotAuth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gh.Close()
	s := newServer(t, gh.URL)
	jl := listen(t, s, "o/r")
	req, _ := http.NewRequest("POST", jl.URL+"twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", strings.NewReader("{}"))
	req.Header.Set("Authorization", mainToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact" || gotAuth != mainToken {
		t.Fatalf("upstream saw path=%q auth=%q", gotPath, gotAuth)
	}
}

func TestScopesFromAuth(t *testing.T) {
	pr := token(Scope{"refs/heads/main", 1}, Scope{"refs/pull/3/merge", 3})
	got := ScopesFromAuth(pr)
	if len(got) != 2 || got[0].Scope != "refs/pull/3/merge" || !got[0].Write() || got[1].Write() {
		t.Fatalf("writable scope should come first: %+v", got)
	}
	for _, bad := range []string{"", "Bearer nope", "Bearer a.!!!.c"} {
		if got := ScopesFromAuth(bad); len(got) != 1 || got[0].Scope != "" || !got[0].Write() {
			t.Errorf("ScopesFromAuth(%q) = %+v, want shared fallback scope", bad, got)
		}
	}
}

// BuildKit's type=gha client (tonistiigi/go-actions-cache) decodes responses
// with encoding/json and the proto field names, and reaches us from a
// container as host.docker.internal, so blob URLs must come back on the host
// it used.
func TestBuildKitClientFieldsAndHost(t *testing.T) {
	s := newServer(t, "")
	jl := listen(t, s, "o/r")
	b, _ := json.Marshal(map[string]any{"key": "buildkit-blob-1", "version": "v"})
	req, _ := http.NewRequest("POST", jl.URL+"twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry", bytes.NewReader(b))
	req.Host = "host.docker.internal:" + strings.Split(strings.TrimSuffix(jl.URL, "/"), ":")[2]
	req.Header.Set("Authorization", mainToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		OK  bool   `json:"ok"`
		URL string `json:"signed_upload_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !created.OK || !strings.HasPrefix(created.URL, "http://"+req.Host+"/u/") {
		t.Fatalf("create = %+v, want an upload URL on %s", created, req.Host)
	}
	// The signature doesn't depend on the host name: upload on 127.0.0.1.
	up := strings.Replace(created.URL, req.Host, strings.TrimPrefix(strings.TrimSuffix(jl.URL, "/"), "http://"), 1)
	put(t, up, []byte("blob"), http.StatusCreated)
	fin := call(t, jl, mainToken, "FinalizeCacheEntryUpload", map[string]any{"key": "buildkit-blob-1", "version": "v", "size_bytes": 4})
	if fin["ok"] != true || fin["entry_id"] == nil {
		t.Fatalf("finalize: %v", fin)
	}
	hit := call(t, jl, mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "buildkit-blob-1", "restore_keys": []string{}, "version": "v"})
	if hit["ok"] != true || hit["matched_key"] != "buildkit-blob-1" || hit["signed_download_url"] == "" {
		t.Fatalf("lookup: %v", hit)
	}
	if code, body := get(t, hit["signed_download_url"].(string)); code != 200 || string(body) != "blob" {
		t.Fatalf("download: %d %q", code, body)
	}
}
