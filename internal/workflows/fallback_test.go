package workflows

import (
	"slices"
	"strings"
	"testing"
)

func TestFallback(t *testing.T) {
	cases := []struct {
		runsOn string
		want   []string
		err    string
	}{
		{`${{ vars.SPARECI_RUNS_ON || 'macos-15' }}`, []string{"macos-15"}, ""},
		{`${{vars.SPARECI_RUNS_ON||'ubuntu-24.04'}}`, []string{"ubuntu-24.04"}, ""},
		{`${{ VARS.spareci_runs_on || 'macos-15' }}`, []string{"macos-15"}, ""}, // expressions are case-insensitive
		{`${{ vars.SPARECI_RUNS_ON || fromJSON('["self-hosted","macOS","ARM64"]') }}`, []string{"self-hosted", "macOS", "ARM64"}, ""},
		{`${{ vars.SPARECI_RUNS_ON || fromjson('"ubuntu-24.04"') }}`, []string{"ubuntu-24.04"}, ""},
		{`${{ vars.SPARECI_RUNS_ON || 'it''s' }}`, []string{"it's"}, ""},
		{`${{ vars.SPARECI_RUNS_ON || matrix.os }}`, nil, "is an expression"},
		{`${{ vars.OTHER || 'macos-15' }}`, nil, "has no vars.SPARECI_RUNS_ON"},
		{`${{ vars.SPARECI_RUNS_ON }}`, nil, "isn't in the form"},
		{`macos-15`, nil, "has no vars.SPARECI_RUNS_ON"},
		{`${{ vars.SPARECI_RUNS_ON || fromJSON('[]') }}`, nil, "isn't a label"},
		{`${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}`, []string{"macos-15"}, ""},
		{`${{ vars.SPARECI_RUNS_ON_LINUX || 'ubuntu-24.04' }}`, []string{"ubuntu-24.04"}, ""},
		{`${{ vars.SPARECI_RUNS_ON_LINUX_ARM || 'ubuntu-24.04-arm' }}`, []string{"ubuntu-24.04-arm"}, ""},
		{`${{ vars.SPARECI_RUNS_ON_WINDOWS || 'windows-2025' }}`, nil, "isn't in the form"},
	}
	for _, c := range cases {
		got, err := Fallback(c.runsOn, DefaultVar)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("Fallback(%s) error = %v, want %q", c.runsOn, err, c.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Fallback(%s) = %q, %v; want %q", c.runsOn, got, err, c.want)
		}
	}
}

const named = `name: CI
on: push
jobs:
  build:
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}
    steps: [{run: make}]
  test:
    name: Unit tests
    strategy:
      matrix:
        node: [20, 22]
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}
    steps: [{run: make test}]
  lint:
    name: Lint ${{ matrix.tool }}
    runs-on: ${{ vars.SPARECI_RUNS_ON || fromJSON('["self-hosted","Linux"]') }}
    steps: [{run: make lint}]
  deploy:
    runs-on: ubuntu-24.04
    steps: [{run: make deploy}]
  matrixed:
    runs-on: ${{ vars.SPARECI_RUNS_ON || matrix.os }}
    steps: [{run: make}]
  call:
    uses: ./.github/workflows/reusable.yml
`

func TestFindJob(t *testing.T) {
	f, err := Parse(".github/workflows/ci.yml", []byte(named), DefaultVar)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		display  string
		id       string
		fallback []string
		fbErr    string
		err      string
	}{
		{"build", "build", []string{"ubuntu-24.04"}, "", ""},
		{"Unit tests (20)", "test", []string{"macos-15"}, "", ""}, // name: plus the matrix values
		{"Unit tests", "test", []string{"macos-15"}, "", ""},
		{"Lint golangci", "lint", []string{"self-hosted", "Linux"}, "", ""}, // expression in name:
		{"deploy", "deploy", nil, "has no vars.SPARECI_RUNS_ON", ""},
		{"matrixed (macos-15)", "matrixed", nil, "is an expression", ""},
		{"test", "", nil, "", "no job named"}, // a job with name: isn't shown by its key
		{"call / build", "", nil, "", "reusable workflow"},
	}
	for _, c := range cases {
		j, err := f.FindJob(c.display)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("FindJob(%q) error = %v, want %q", c.display, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("FindJob(%q): %v", c.display, err)
			continue
		}
		if j.ID != c.id || !slices.Equal(j.Fallback, c.fallback) || !strings.Contains(j.FallbackErr, c.fbErr) {
			t.Errorf("FindJob(%q) = %s fallback %q (%s); want %s %q (%s)", c.display, j.ID, j.Fallback, j.FallbackErr, c.id, c.fallback, c.fbErr)
		}
	}
}

func TestFindJobAmbiguous(t *testing.T) {
	src := `jobs:
  a:
    name: Build
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}
  b:
    name: Build
    runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}
`
	f, err := Parse("ci.yml", []byte(src), DefaultVar)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.FindJob("Build"); err == nil || !strings.Contains(err.Error(), "could be any of a, b") {
		t.Errorf("FindJob of a duplicated name = %v, want an ambiguity error", err)
	}
}
