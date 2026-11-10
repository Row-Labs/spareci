// Package cache is a local stand-in for GitHub's Actions cache service, so
// actions/cache (and setup-node, setup-go, ... which embed it) read and write
// the machine's own disk instead of uploading to GitHub and back.
package cache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one saved cache archive.
type Entry struct {
	ID       int64     `json:"id"`
	Repo     string    `json:"repo"`
	Scope    string    `json:"scope"` // git ref the entry was saved from, e.g. refs/heads/main
	Key      string    `json:"key"`
	Version  string    `json:"version"` // hash of paths + compression; must match exactly
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
}

type upload struct {
	id                        string
	repo, scope, key, version string
	created                   time.Time
}

// ErrExists means an entry (or an in-flight upload) already has this key and
// version in this scope. GitHub's cache is immutable per key, and so is ours.
var ErrExists = errors.New("cache entry already exists")

// Store keeps archives on disk with a JSON index. It is shared by every job.
type Store struct {
	dir      string
	maxBytes int64

	mu      sync.Mutex
	entries map[int64]*Entry
	nextID  int64
	uploads map[string]*upload
}

type index struct {
	NextID  int64    `json:"next_id"`
	Entries []*Entry `json:"entries"`
}

func Open(dir string, maxBytes int64) (*Store, error) {
	s := &Store{dir: dir, maxBytes: maxBytes, entries: map[int64]*Entry{}, nextID: 1, uploads: map[string]*upload{}}
	for _, d := range []string{s.blobDir(), s.uploadDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Uploads never survive a restart: the job that started them is gone.
	_ = os.RemoveAll(s.uploadDir())
	_ = os.MkdirAll(s.uploadDir(), 0o755)

	data, err := os.ReadFile(s.indexPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		var idx index
		if err := json.Unmarshal(data, &idx); err != nil {
			return nil, fmt.Errorf("read cache index: %w", err)
		}
		s.nextID = max(idx.NextID, 1)
		for _, e := range idx.Entries {
			if _, err := os.Stat(s.blobPath(e.ID)); err == nil {
				s.entries[e.ID] = e
			}
		}
	}
	return s, nil
}

func (s *Store) indexPath() string        { return filepath.Join(s.dir, "index.json") }
func (s *Store) blobDir() string          { return filepath.Join(s.dir, "blobs") }
func (s *Store) uploadDir() string        { return filepath.Join(s.dir, "uploads") }
func (s *Store) blobPath(id int64) string { return filepath.Join(s.blobDir(), fmt.Sprint(id)) }
func (s *Store) uploadPath(id string) string {
	return filepath.Join(s.uploadDir(), id)
}

// saveLocked writes the index atomically. Caller holds s.mu.
func (s *Store) saveLocked() error {
	idx := index{NextID: s.nextID}
	for _, e := range s.entries {
		idx.Entries = append(idx.Entries, e)
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].ID < idx.Entries[j].ID })
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

// Lookup finds the entry actions/cache would restore, following GitHub's
// rules: in each scope in order (the job's own ref first, then the refs it may
// read, such as a PR's base branch), try the key exactly, then as a prefix,
// then each restore key as a prefix, newest entry first. Version must match.
func (s *Store) Lookup(repo string, scopes []string, key string, restoreKeys []string, version string) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range scopes {
		var cands []*Entry
		for _, e := range s.entries {
			if e.Repo == repo && e.Scope == scope && e.Version == version {
				cands = append(cands, e)
			}
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].Created.After(cands[j].Created) })
		match := func(pred func(string) bool) *Entry {
			for _, e := range cands {
				if pred(e.Key) {
					return e
				}
			}
			return nil
		}
		found := match(func(k string) bool { return k == key })
		if found == nil {
			found = match(func(k string) bool { return strings.HasPrefix(k, key) })
		}
		for _, rk := range restoreKeys {
			if found != nil {
				break
			}
			if rk = strings.TrimSpace(rk); rk != "" {
				found = match(func(k string) bool { return strings.HasPrefix(k, rk) })
			}
		}
		if found != nil {
			found.LastUsed = time.Now()
			_ = s.saveLocked()
			cp := *found
			return &cp
		}
	}
	return nil
}

// Reserve starts an upload for key+version in scope and returns its id.
func (s *Store) Reserve(repo, scope, key, version string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Repo == repo && e.Scope == scope && e.Key == key && e.Version == version {
			return "", ErrExists
		}
	}
	for id, u := range s.uploads {
		if time.Since(u.created) > time.Hour { // abandoned
			delete(s.uploads, id)
			_ = os.RemoveAll(s.uploadPath(id))
			continue
		}
		if u.repo == repo && u.scope == scope && u.key == key && u.version == version {
			return "", ErrExists
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Join(s.uploadPath(id), "blocks"), 0o755); err != nil {
		return "", err
	}
	s.uploads[id] = &upload{id: id, repo: repo, scope: scope, key: key, version: version, created: time.Now()}
	return id, nil
}

func (s *Store) pending(repo, id string) (*upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[id]
	if u == nil || u.repo != repo {
		return nil, fmt.Errorf("no upload %q", id)
	}
	return u, nil
}

// PutBlob writes the whole archive in one request.
func (s *Store) PutBlob(repo, id string, r io.Reader) (int64, error) {
	if _, err := s.pending(repo, id); err != nil {
		return 0, err
	}
	return writeFile(filepath.Join(s.uploadPath(id), "data"), r)
}

// PutBlock writes one block of a multi-part archive.
func (s *Store) PutBlock(repo, id, blockID string, r io.Reader) (int64, error) {
	if _, err := s.pending(repo, id); err != nil {
		return 0, err
	}
	return writeFile(s.blockPath(id, blockID), r)
}

func (s *Store) blockPath(id, blockID string) string {
	return filepath.Join(s.uploadPath(id), "blocks", hex.EncodeToString([]byte(blockID)))
}

// CommitBlocks joins blocks, in the given order, into the archive.
func (s *Store) CommitBlocks(repo, id string, blockIDs []string) error {
	if _, err := s.pending(repo, id); err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(s.uploadPath(id), "data"))
	if err != nil {
		return err
	}
	defer out.Close()
	for _, b := range blockIDs {
		in, err := os.Open(s.blockPath(id, b))
		if err != nil {
			return fmt.Errorf("block %q: %w", b, err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	_ = os.RemoveAll(filepath.Join(s.uploadPath(id), "blocks"))
	return out.Close()
}

// Finalize turns the upload for key+version into a restorable entry.
func (s *Store) Finalize(repo, scope, key, version string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var u *upload
	for _, c := range s.uploads {
		if c.repo == repo && c.scope == scope && c.key == key && c.version == version {
			u = c
			break
		}
	}
	if u == nil {
		return nil, fmt.Errorf("no upload for key %q", key)
	}
	data := filepath.Join(s.uploadPath(u.id), "data")
	fi, err := os.Stat(data)
	if err != nil {
		return nil, fmt.Errorf("upload for key %q has no data", key)
	}
	e := &Entry{ID: s.nextID, Repo: repo, Scope: scope, Key: key, Version: version, Size: fi.Size(), Created: time.Now(), LastUsed: time.Now()}
	if err := os.Rename(data, s.blobPath(e.ID)); err != nil {
		return nil, err
	}
	s.nextID++
	s.entries[e.ID] = e
	delete(s.uploads, u.id)
	_ = os.RemoveAll(s.uploadPath(u.id))
	s.evictLocked(e.ID)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *e
	return &cp, nil
}

// evictLocked drops least recently used entries until the store fits, never
// the one just saved.
func (s *Store) evictLocked(keep int64) {
	if s.maxBytes <= 0 {
		return
	}
	var total int64
	all := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		total += e.Size
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].LastUsed.Before(all[j].LastUsed) })
	for _, e := range all {
		if total <= s.maxBytes {
			return
		}
		if e.ID == keep {
			continue
		}
		slog.Info("cache evict", "repo", e.Repo, "key", e.Key, "size", HumanBytes(e.Size))
		_ = os.Remove(s.blobPath(e.ID))
		delete(s.entries, e.ID)
		total -= e.Size
	}
}

// OpenBlob opens an entry's archive, checking it belongs to repo.
func (s *Store) OpenBlob(repo string, id int64) (*os.File, error) {
	s.mu.Lock()
	e := s.entries[id]
	s.mu.Unlock()
	if e == nil || e.Repo != repo {
		return nil, os.ErrNotExist
	}
	return os.Open(s.blobPath(id))
}

// List returns entries, newest first; repo "" means all.
func (s *Store) List(repo string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries {
		if repo == "" || e.Repo == repo {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Delete removes entries for repo ("" for all) and returns how many.
func (s *Store) Delete(repo string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, e := range s.entries {
		if repo == "" || e.Repo == repo {
			_ = os.Remove(s.blobPath(id))
			delete(s.entries, id)
			n++
		}
	}
	return n, s.saveLocked()
}

func writeFile(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// HumanBytes formats a size like "1.5 GB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ParseBytes reads sizes like "10GB", "512MB", "2g" or a plain byte count.
func ParseBytes(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B")
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			s = s[:len(s)-1]
		}
	}
	var n float64
	if _, err := fmt.Sscanf(s, "%g", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}
