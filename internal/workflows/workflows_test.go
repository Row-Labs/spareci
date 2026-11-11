package workflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	mac = Machine{OS: "darwin", Arch: "arm64", Labels: []string{"self-hosted", "macOS", "ARM64", "spareci"}}
	// A Mac with a container runtime, taking arm64 Linux jobs and x64 ones.
	macLinux = Machine{OS: "darwin", Arch: "arm64", Labels: mac.Labels, Linux: []string{"arm64", "x64"}}
	macARM   = Machine{OS: "darwin", Arch: "arm64", Labels: mac.Labels, Linux: []string{"arm64"}}
	linux    = Machine{OS: "linux", Arch: "amd64", Labels: []string{"self-hosted", "Linux", "X64", "spareci"}}
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
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}\n    steps: []\n",
		},
		{
			name: "trailing comment kept",
			in:   "jobs:\n  build:\n    runs-on: macos-15   # 10x minutes\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}   # 10x minutes\n",
		},
		{
			name: "double quoted stays quoted",
			in:   "jobs:\n  build:\n    runs-on: \"ubuntu-latest\"\n",
			want: "jobs:\n  build:\n    runs-on: \"${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-latest' }}\"\n",
		},
		{
			name: "single quoted",
			in:   "jobs:\n  build:\n    runs-on: 'ubuntu-latest' # hi\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-latest' }} # hi\n",
		},
		{
			name: "one-item flow list",
			in:   "jobs:\n  build:\n    runs-on: [ubuntu-latest]\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-latest' }}\n",
		},
		{
			name: "self-hosted flow list keeps every label",
			in:   "jobs:\n  build:\n    runs-on: [self-hosted, macOS, ARM64]\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || fromJSON('[\"self-hosted\",\"macOS\",\"ARM64\"]') }}\n",
		},
		{
			name: "block list collapses to one line",
			in:   "jobs:\n  build:\n    runs-on: # where\n      - self-hosted\n      - macOS\n    steps: []\n",
			want: "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || fromJSON('[\"self-hosted\",\"macOS\"]') }} # where\n    steps: []\n",
		},
		{
			name: "CRLF",
			in:   "jobs:\r\n  build:\r\n    runs-on: ubuntu-24.04\r\n    steps: []\r\n",
			want: "jobs:\r\n  build:\r\n    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}\r\n    steps: []\r\n",
		},
		{
			name: "label with a quote",
			in:   "jobs:\n  build:\n    runs-on: \"it's-mine\"\n",
			want: "jobs:\n  build:\n    runs-on: \"${{ vars.SPARECI_RUNS_ON_MACOS || 'it''s-mine' }}\"\n",
		},
		{
			name: "already converted",
			in:   "jobs:\n  build:\n    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}\n",
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
    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || fromJSON('["self-hosted","macOS"]') }}
    steps: []
  b:
    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}
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
		{"ubuntu-24.04", macLinux, FitGood},
		{"ubuntu-24.04-arm", macLinux, FitGood},
		{"ubuntu-24.04", macARM, FitMaybe},
		{"ubuntu-24.04-arm", macARM, FitGood},
		{"[self-hosted, Linux, ARM64]", macARM, FitGood},
		{"[self-hosted, Linux, X64]", macARM, FitNo},
		{"[self-hosted, Linux, macOS]", macLinux, FitNo},
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
	if j := parse(t, src, macLinux).Jobs[0]; j.Fit != FitNo {
		t.Errorf("container job on a Mac with containers: Fit = %d", j.Fit)
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

// TestAppWorkflows runs over a realistic CI and release pair: on a Mac only
// the macOS jobs are recommended, and converting them changes just their
// runs-on lines.
func TestAppWorkflows(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "release.yml"} {
		b, err := os.ReadFile(filepath.Join("testdata", "app-"+name))
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
	if s := find(files, "ci", "smoke").Skip; !strings.Contains(s, "matrix") {
		t.Errorf("ci:smoke Skip = %q, want the matrix reason", s)
	}

	mac.Assess(files)
	rec := Recommended(files)
	if got := names(rec); got != "ci:ios,release:build-macos,release:sign-macos" {
		t.Fatalf("Recommended on a Mac = %s", got)
	}
	changes, err := Apply(files, rec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		".github/workflows/ci.yml": {
			"+    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}",
		},
		".github/workflows/release.yml": {
			"+    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }} # needs Xcode",
			"+    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}",
		},
	}
	if len(changes) != len(want) {
		t.Fatalf("%d files changed, want %d", len(changes), len(want))
	}
	for _, c := range changes {
		diff := Diff(c.File.Path, c.File.Src, c.New)
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
		if len(minus) != len(want[c.File.Path]) || strings.Join(plus, "\n") != strings.Join(want[c.File.Path], "\n") {
			t.Errorf("%s: diff isn't just the runs-on lines:\n%s", c.File.Path, diff)
		}
	}

	linux.Assess(files)
	if got := names(Recommended(files)); got != "ci:changes,ci:checks,ci:tests,release:checks,release:build-linux,release:publish" {
		t.Errorf("Recommended on Linux = %s", got)
	}
}

// Each job gets the variable for the runner it falls back to.
func TestRewriteVariablePerOS(t *testing.T) {
	cases := []struct {
		runsOn string
		m      Machine
		want   string
	}{
		{"macos-15", mac, "SPARECI_RUNS_ON_MACOS"},
		{"ubuntu-24.04", mac, "SPARECI_RUNS_ON_LINUX"},
		{"ubuntu-24.04-arm", mac, "SPARECI_RUNS_ON_LINUX_ARM"},
		{"[self-hosted, Linux, ARM64]", linux, "SPARECI_RUNS_ON_LINUX_ARM"},
		{"[self-hosted, Linux]", linux, "SPARECI_RUNS_ON_LINUX"},
		{"self-hosted", mac, "SPARECI_RUNS_ON_MACOS"},                                       // this machine's own
		{"my-big-runner", linux, "SPARECI_RUNS_ON_LINUX"},                                   // x64 Linux host
		{"my-big-runner", Machine{OS: "linux", Arch: "arm64"}, "SPARECI_RUNS_ON_LINUX_ARM"}, // arm64 Linux host
	}
	for _, c := range cases {
		f := parse(t, "jobs:\n  j:\n    runs-on: "+c.runsOn+"\n", c.m)
		got, err := f.Rewrite(f.Jobs)
		if err != nil {
			t.Fatalf("%s: %v", c.runsOn, err)
		}
		if !strings.Contains(string(got), "vars."+c.want+" ||") {
			t.Errorf("%s on %s/%s: %s, want %s", c.runsOn, c.m.OS, c.m.Arch, got, c.want)
		}
	}
}

// Jobs routed by the old shared variable are offered the per-OS one.
func TestUpgrade(t *testing.T) {
	in := `jobs:
  mac:
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}
  linux:
    runs-on: "${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}" # keep me
  arm:
    runs-on: ${{ vars.SPARECI_RUNS_ON || fromJSON('["self-hosted","Linux","ARM64"]') }}
  custom:
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'my-big-runner' }}
  done:
    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}
`
	want := `jobs:
  mac:
    runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}
  linux:
    runs-on: "${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}" # keep me
  arm:
    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX_ARM || fromJSON('["self-hosted","Linux","ARM64"]') }}
  custom:
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'my-big-runner' }}
  done:
    runs-on: ${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}
`
	f := parse(t, in, mac)
	up := Upgradable([]*File{f})
	var names []string
	for _, j := range up {
		names = append(names, j.ID)
	}
	if strings.Join(names, ",") != "mac,linux,arm" {
		t.Fatalf("upgradable = %v, want mac, linux, arm (custom's OS is unknown; done is done)", names)
	}
	if j := job(t, f, "done"); j.RoutedBy != "SPARECI_RUNS_ON_LINUX" || j.Upgrade {
		t.Errorf("done: RoutedBy %q Upgrade %v", j.RoutedBy, j.Upgrade)
	}
	got, err := f.Rewrite(up)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if again := Upgradable([]*File{parse(t, string(got), mac)}); len(again) != 0 {
		t.Errorf("still upgradable after upgrading: %v", again)
	}
}

func TestWatchdogIsNeverConverted(t *testing.T) {
	f := parse(t, "# Generated by `spareci watchdog`. Keep it on the default branch.\njobs:\n  watchdog:\n    runs-on: ubuntu-latest\n", macLinux)
	if j := f.Jobs[0]; !strings.Contains(j.Skip, "watchdog") {
		t.Errorf("watchdog job Skip = %q", j.Skip)
	}
}
