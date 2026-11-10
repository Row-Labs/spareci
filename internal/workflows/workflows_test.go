package workflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	mac   = Machine{OS: "darwin", Arch: "arm64", Labels: []string{"self-hosted", "macOS", "ARM64", "spareci"}}
	linux = Machine{OS: "linux", Arch: "amd64", Labels: []string{"self-hosted", "Linux", "X64", "spareci"}}
)

func parse(t *testing.T, src string, m Machine) *File {
	t.Helper()
	f, err := Parse(".github/workflows/ci.yml", []byte(src), DefaultVar)
	if err != nil {
		t.Fatal(err)
	}
	m.Assess([]*File{f})
	return f
}

func job(t *testing.T, f *File, id string) *Job {
	t.Helper()
	for _, j := range f.Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("no job %s", id)
	return nil
}

// TestRewrite: converting one job changes exactly its runs-on and leaves the
// rest of the file byte for byte.
func TestRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" when the job is skipped
		skip string // substring of Skip
	}{
		{
			name: "plain",
			in:   "jobs:\n  build:\n    runs-on: ubuntu-24.04\n    steps: []\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}\n    steps: []\n",
		},
		{
			name: "trailing comment kept",
			in:   "jobs:\n  build:\n    runs-on: macos-15   # 10x minutes\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}   # 10x minutes\n",
		},
		{
			name: "double quoted stays quoted",
			in:   "jobs:\n  build:\n    runs-on: \"ubuntu-latest\"\n",
			want: "jobs:\n  build:\n    runs-on: \"${{ vars.SPARECI_RUNS_ON || 'ubuntu-latest' }}\"\n",
		},
		{
			name: "single quoted",
			in:   "jobs:\n  build:\n    runs-on: 'ubuntu-latest' # hi\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-latest' }} # hi\n",
		},
		{
			name: "one-item flow list",
			in:   "jobs:\n  build:\n    runs-on: [ubuntu-latest]\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-latest' }}\n",
		},
		{
			name: "self-hosted flow list keeps every label",
			in:   "jobs:\n  build:\n    runs-on: [self-hosted, macOS, ARM64]\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || fromJSON('[\"self-hosted\",\"macOS\",\"ARM64\"]') }}\n",
		},
		{
			name: "block list collapses to one line",
			in:   "jobs:\n  build:\n    runs-on: # where\n      - self-hosted\n      - macOS\n    steps: []\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || fromJSON('[\"self-hosted\",\"macOS\"]') }} # where\n    steps: []\n",
		},
		{
			name: "CRLF",
			in:   "jobs:\r\n  build:\r\n    runs-on: ubuntu-24.04\r\n    steps: []\r\n",
			want: "jobs:\r\n  build:\r\n    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}\r\n    steps: []\r\n",
		},
		{
			name: "label with a quote",
			in:   "jobs:\n  build:\n    runs-on: \"it's-mine\"\n",
			want: "jobs:\n  build:\n    runs-on: \"${{ vars.SPARECI_RUNS_ON || 'it''s-mine' }}\"\n",
		},
		{
			name: "already converted",
			in:   "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}\n",
			skip: "already routed",
		},
		{
			name: "matrix",
			in:   "jobs:\n  build:\n    strategy:\n      matrix:\n        os: [ubuntu-latest, macos-15]\n    runs-on: ${{ matrix.os }}\n",
			skip: "matrix",
		},
		{
			name: "matrix in a list",
			in:   "jobs:\n  build:\n    runs-on: [self-hosted, \"${{ matrix.arch }}\"]\n",
			skip: "matrix",
		},
		{
			name: "other expression",
			in:   "jobs:\n  build:\n    runs-on: ${{ github.repository_owner == 'x' && 'big' || 'ubuntu-latest' }}\n",
			skip: "expression",
		},
		{
			name: "group",
			in:   "jobs:\n  build:\n    runs-on:\n      group: ubuntu-runners\n      labels: ubuntu-20.04-16core\n",
			skip: "group",
		},
		{
			name: "reusable workflow",
			in:   "jobs:\n  call:\n    uses: ./.github/workflows/build.yml\n",
			skip: "reusable",
		},
		{
			name: "alias",
			in:   "x: &os ubuntu-latest\njobs:\n  build:\n    runs-on: *os\n",
			skip: "alias",
		},
		{
			name: "flow list over lines",
			in:   "jobs:\n  build:\n    runs-on: [self-hosted,\n      macOS]\n",
			skip: "spans lines",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := parse(t, c.in, mac)
			j := f.Jobs[len(f.Jobs)-1]
			if c.skip != "" {
				if !strings.Contains(j.Skip, c.skip) {
					t.Fatalf("Skip = %q, want it to mention %q", j.Skip, c.skip)
				}
				if _, err := f.Rewrite([]*Job{j}); err == nil {
					t.Fatal("Rewrite of a skipped job succeeded")
				}
				return
			}
			if j.Skip != "" {
				t.Fatalf("skipped: %s", j.Skip)
			}
			got, err := f.Rewrite([]*Job{j})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
			// The result must still be valid YAML with the expression as
			// runs-on, and converting it again must be a no-op.
			again := parse(t, string(got), mac)
			if s := job(t, again, j.ID).Skip; !strings.Contains(s, "already routed") {
				t.Fatalf("after rewrite, Skip = %q", s)
			}
		})
	}
}

func TestRewriteSeveral(t *testing.T) {
	in := `jobs:
  a:
    runs-on:
      - self-hosted
      - macOS
    steps: []
  b:
    runs-on: macos-15
  c:
    runs-on: ubuntu-latest
`
	want := `jobs:
  a:
    runs-on: ${{ vars.SPARECI_RUNS_ON || fromJSON('["self-hosted","macOS"]') }}
    steps: []
  b:
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}
  c:
    runs-on: ubuntu-latest
`
	f := parse(t, in, mac)
	got, err := f.Rewrite([]*Job{job(t, f, "b"), job(t, f, "a")})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFit(t *testing.T) {
	cases := []struct {
		runsOn string
		m      Machine
		want   Fit
	}{
		{"macos-15", mac, FitGood},
		{"macos-latest", mac, FitGood},
		{"macos-14-xlarge", mac, FitGood},
		{"macos-13", mac, FitMaybe},       // Intel
		{"macos-15-intel", mac, FitMaybe}, // Intel
		{"ubuntu-24.04", mac, FitMaybe},
		{"windows-latest", mac, FitNo},
		{"my-big-runner", mac, FitMaybe},
		{"[self-hosted, macOS, ARM64]", mac, FitGood},
		{"[self-hosted, macOS, gpu]", mac, FitNo},
		{"self-hosted", mac, FitGood},
		{"ubuntu-24.04", linux, FitGood},
		{"ubuntu-latest", linux, FitGood},
		{"ubuntu-24.04-arm", linux, FitMaybe},
		{"macos-15", linux, FitNo},
		{"[self-hosted, linux]", linux, FitGood},
		{"[ubuntu-latest, macos-15]", linux, FitNo},
	}
	for _, c := range cases {
		f := parse(t, "jobs:\n  j:\n    runs-on: "+c.runsOn+"\n", c.m)
		j := f.Jobs[0]
		if j.Skip != "" {
			t.Fatalf("%s: skipped: %s", c.runsOn, j.Skip)
		}
		if j.Fit != c.want {
			t.Errorf("%s on %s: Fit = %d (%s), want %d", c.runsOn, c.m.OS, j.Fit, j.Why, c.want)
		}
	}

	// Containers need Linux.
	src := "jobs:\n  j:\n    runs-on: ubuntu-latest\n    container: node:22\n"
	if j := parse(t, src, mac).Jobs[0]; j.Fit != FitNo {
		t.Errorf("container job on a Mac: Fit = %d", j.Fit)
	}
	if j := parse(t, src, linux).Jobs[0]; j.Fit != FitGood {
		t.Errorf("container job on Linux: Fit = %d (%s)", j.Fit, j.Why)
	}
}

func TestSelect(t *testing.T) {
	f := parse(t, `jobs:
  build:
    runs-on: macos-15
  lint:
    runs-on: ubuntu-latest
  win:
    runs-on: windows-latest
  m:
    runs-on: ${{ matrix.os }}
`, mac)
	files := []*File{f}
	if got := names(Recommended(files)); got != "ci:build" {
		t.Errorf("Recommended = %s", got)
	}
	if got := names(Convertible(files)); got != "ci:build,ci:lint" {
		t.Errorf("Convertible = %s", got)
	}
	got, err := Select(files, "ci:lint, ci.yml:build,ci:lint")
	if err != nil || names(got) != "ci:lint,ci:build" {
		t.Errorf("Select = %s, %v", names(got), err)
	}
	for spec, want := range map[string]string{
		"ci:win":   "Windows",
		"ci:m":     "matrix",
		"ci:nope":  "no job",
		"build":    "workflow:job",
		"other:ci": "no job",
		"":         "no jobs",
	} {
		if _, err := Select(files, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Select(%q) err = %v, want %q", spec, err, want)
		}
	}
}

func names(jobs []*Job) string {
	var s []string
	for _, j := range jobs {
		s = append(s, j.Name())
	}
	return strings.Join(s, ",")
}

func TestDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\n"
	new := strings.Replace(strings.Replace(old, "c\n", "C\n", 1), "m\n", "M\n", 1)
	want := `--- a/x.yml
+++ b/x.yml
@@ -1,6 +1,6 @@
 a
 b
-c
+C
 d
 e
 f
@@ -10,5 +10,5 @@
 j
 k
 l
-m
+M
 n
`
	if got := Diff("x.yml", []byte(old), []byte(new)); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	if Diff("x", []byte(old), []byte(old)) != "" {
		t.Error("diff of equal files isn't empty")
	}
}

// TestRelay runs over copies of Row-Labs/relay's workflows: on a Mac, only the
// macOS jobs are recommended, and converting them changes just those lines.
func TestRelay(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "release.yml"} {
		b, err := os.ReadFile(filepath.Join("testdata", "relay-"+name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := Scan(root, DefaultVar)
	if err != nil {
		t.Fatal(err)
	}

	mac.Assess(files)
	rec := Recommended(files)
	if got := names(rec); got != "release:bundles-macos,release:desktop-macos" {
		t.Fatalf("Recommended on a Mac = %s", got)
	}
	changes, err := Apply(files, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].File.Path != ".github/workflows/release.yml" {
		t.Fatalf("changes = %v", changes)
	}
	diff := Diff(changes[0].File.Path, changes[0].File.Src, changes[0].New)
	var minus, plus []string
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
		case strings.HasPrefix(l, "-"):
			minus = append(minus, l)
		case strings.HasPrefix(l, "+"):
			plus = append(plus, l)
		}
	}
	wantPlus := "+    runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}"
	if len(minus) != 2 || len(plus) != 2 || plus[0] != wantPlus || plus[1] != wantPlus {
		t.Fatalf("diff isn't two runs-on lines:\n%s", diff)
	}

	linux.Assess(files)
	if got := len(Recommended(files)); got != 8 {
		t.Errorf("Recommended on Linux: %d jobs, want 8: %s", got, names(Recommended(files)))
	}
}
