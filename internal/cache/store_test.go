package cache

import (
	"strings"
	"testing"
	"time"
)

func save(t *testing.T, s *Store, repo, scope, key, version, body string) *Entry {
	t.Helper()
	id, err := s.Reserve(repo, scope, key, version)
	if err != nil {
		t.Fatalf("reserve %s: %v", key, err)
	}
	if _, err := s.PutBlob(repo, id, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	e, err := s.Finalize(repo, scope, key, version)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct Created for newest-first ordering
	return e
}

func TestLookupFollowsGitHubRules(t *testing.T) {
	s, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	const r, v = "o/r", "v1"
	save(t, s, r, "refs/heads/main", "deps-linux-aaa", v, "main-aaa")
	save(t, s, r, "refs/heads/main", "deps-linux-bbb", v, "main-bbb")
	save(t, s, r, "refs/pull/7/merge", "deps-linux-ccc", v, "pr-ccc")
	save(t, s, r, "refs/heads/main", "other", "v2", "other-v2")
	save(t, s, "o/elsewhere", "refs/heads/main", "deps-linux-zzz", v, "elsewhere")

	pr := []string{"refs/pull/7/merge", "refs/heads/main"}
	main := []string{"refs/heads/main"}
	cases := []struct {
		name    string
		scopes  []string
		key     string
		restore []string
		version string
		want    string // matched key, "" for a miss
	}{
		{"exact", main, "deps-linux-aaa", nil, v, "deps-linux-aaa"},
		{"key as prefix, newest", main, "deps-linux-", nil, v, "deps-linux-bbb"},
		{"restore key", main, "deps-linux-new", []string{"deps-linux-"}, v, "deps-linux-bbb"},
		{"own scope first", pr, "deps-linux-new", []string{"deps-linux-"}, v, "deps-linux-ccc"},
		{"falls back to base branch", pr, "deps-linux-aaa", nil, v, "deps-linux-aaa"},
		{"base cannot see PR", main, "deps-linux-ccc", nil, v, ""},
		{"version must match", main, "other", nil, v, ""},
		{"other repos invisible", main, "deps-linux-zzz", nil, v, ""},
		{"miss", main, "nope", []string{"also-no"}, v, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := s.Lookup(r, c.scopes, c.key, c.restore, c.version)
			got := ""
			if e != nil {
				got = e.Key
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestReserveIsImmutablePerKey(t *testing.T) {
	s, _ := Open(t.TempDir(), 0)
	if _, err := s.Reserve("o/r", "refs/heads/main", "k", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("o/r", "refs/heads/main", "k", "v"); err != ErrExists {
		t.Fatalf("second reserve while uploading: got %v, want ErrExists", err)
	}
	save(t, s, "o/r", "refs/heads/main", "k2", "v", "x")
	if _, err := s.Reserve("o/r", "refs/heads/main", "k2", "v"); err != ErrExists {
		t.Fatalf("reserve of saved key: got %v, want ErrExists", err)
	}
	if _, err := s.Reserve("o/r", "refs/heads/dev", "k2", "v"); err != nil {
		t.Fatalf("same key on another branch should be allowed: %v", err)
	}
}

func TestBlocksCommitInListOrder(t *testing.T) {
	s, _ := Open(t.TempDir(), 0)
	id, _ := s.Reserve("o/r", "", "k", "v")
	for _, b := range []struct{ id, data string }{{"Yg==", "BBB"}, {"YQ==", "AAA"}, {"Yw==", "CCC"}} {
		if _, err := s.PutBlock("o/r", id, b.id, strings.NewReader(b.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CommitBlocks("o/r", id, []string{"YQ==", "Yg==", "Yw=="}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Finalize("o/r", "", "k", "v")
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != 9 {
		t.Fatalf("size %d, want 9", e.Size)
	}
	f, _ := s.OpenBlob("o/r", e.ID)
	defer f.Close()
	buf := make([]byte, 9)
	_, _ = f.Read(buf)
	if string(buf) != "AAABBBCCC" {
		t.Fatalf("got %q", buf)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	s, _ := Open(t.TempDir(), 10)
	save(t, s, "o/r", "", "old", "v", "aaaa")
	save(t, s, "o/r", "", "used", "v", "bbbb")
	s.Lookup("o/r", []string{""}, "old", nil, "v") // "old" is now most recently used
	save(t, s, "o/r", "", "new", "v", "cccc")      // 12 bytes > 10: evict "used"
	keys := map[string]bool{}
	for _, e := range s.List("") {
		keys[e.Key] = true
	}
	if !keys["old"] || !keys["new"] || keys["used"] {
		t.Fatalf("after eviction: %v", keys)
	}
}

func TestIndexSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 0)
	save(t, s, "o/r", "", "k", "v", "data")
	s2, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e := s2.Lookup("o/r", []string{""}, "k", nil, "v"); e == nil || e.Size != 4 {
		t.Fatalf("entry lost on restart: %+v", e)
	}
	if id, _ := s2.Reserve("o/r", "", "k3", "v"); id == "" {
		t.Fatal("no upload id")
	}
	if e := save(t, s2, "o/r", "", "k4", "v", "x"); e.ID <= 1 {
		t.Fatalf("ids restarted: %d", e.ID)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"10GB": 10 << 30, "512mb": 512 << 20, "2g": 2 << 30, "1.5GiB": 3 << 29, "100": 100} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Error("ParseBytes(lots) should fail")
	}
}
