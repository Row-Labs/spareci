// Package workflows finds the jobs in a repo's GitHub Actions workflows and
// rewrites their runs-on so they run on spareci while it's up:
//
//	runs-on: ubuntu-24.04
//	runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}
//
// The YAML is parsed only to find where each runs-on is. The rewrite is a line
// edit on the original text, so comments, quoting and layout elsewhere stay as
// they were.
package workflows

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Dir is where GitHub looks for workflows, relative to the repo root.
const Dir = ".github/workflows"

// DefaultVar is the repo variable spareci sets while it's up.
const DefaultVar = "SPARECI_RUNS_ON"

// File is one workflow file and its jobs.
type File struct {
	Path string // relative to the repo root, e.g. .github/workflows/ci.yml
	Src  []byte
	Jobs []*Job
}

// Job is one job in a workflow and what spareci can do with its runs-on.
type Job struct {
	Workflow string // file name without extension: the "ci" in ci:build
	ID       string // job key: the "build" in ci:build
	Line     int    // 1-based line of runs-on
	RunsOn   string // runs-on as written, for display
	Labels   []string
	// Skip says why this job can't be converted. Empty when it can.
	Skip string
	// Fit and Why say whether this machine suits the job, set by Assess.
	Fit Fit
	Why string

	container bool // uses container: or services:
	edit      *edit
}

// Name is how the job is named on the command line: workflow:job.
func (j *Job) Name() string { return j.Workflow + ":" + j.ID }

// edit replaces lines [start, end) (0-based) with lines.
type edit struct {
	start, end int
	lines      []string
}

// Scan reads every .yml and .yaml file in root/.github/workflows.
func Scan(root, varName string) ([]*File, error) {
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if err != nil {
		return nil, err
	}
	var files []*File
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(Dir, e.Name()))
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		f, err := Parse(rel, src, varName)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// Parse finds the jobs in one workflow file.
func Parse(path string, src []byte, varName string) (*File, error) {
	f := &File{Path: path, Src: src}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return f, nil
	}
	jobs := lookup(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return f, nil
	}
	lines := splitLines(src)
	workflow := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		key, body := jobs.Content[i], jobs.Content[i+1]
		j := &Job{Workflow: workflow, ID: key.Value, Line: key.Line}
		f.Jobs = append(f.Jobs, j)
		if body.Kind != yaml.MappingNode {
			j.Skip = "not a job mapping"
			continue
		}
		j.container = lookup(body, "container") != nil || lookup(body, "services") != nil
		k, v := lookupPair(body, "runs-on")
		if v == nil {
			if lookup(body, "uses") != nil {
				j.Skip = "calls a reusable workflow; convert the jobs inside it"
			} else {
				j.Skip = "no runs-on"
			}
			continue
		}
		j.Line = k.Line
		j.RunsOn = strings.TrimSpace(sourceText(lines, k, v))
		j.Labels, j.Skip = labels(v, varName)
		if j.Skip != "" {
			continue
		}
		e, err := makeEdit(lines, k, v, rewritten(j.Labels, varName))
		if err != nil {
			j.Skip = err.Error()
			continue
		}
		j.edit = e
	}
	return f, nil
}

// labels returns a runs-on's static labels, or why there aren't any.
func labels(v *yaml.Node, varName string) ([]string, string) {
	expr := func(s string) string {
		switch {
		case strings.Contains(s, "vars."+varName):
			return "already routed to spareci"
		case strings.Contains(s, "matrix."):
			return "runs-on comes from the matrix; spareci can't tell which entries this machine fits, so edit it by hand"
		default:
			return "runs-on is an expression; edit it by hand"
		}
	}
	switch v.Kind {
	case yaml.ScalarNode:
		if strings.Contains(v.Value, "${{") {
			return nil, expr(v.Value)
		}
		if strings.TrimSpace(v.Value) == "" || strings.ContainsAny(v.Value, "\n") {
			return nil, "runs-on is empty or spans lines"
		}
		return []string{v.Value}, ""
	case yaml.SequenceNode:
		var out []string
		for _, it := range v.Content {
			if it.Kind != yaml.ScalarNode || strings.ContainsAny(it.Value, "\n") {
				return nil, "runs-on list has an entry that isn't a plain label"
			}
			if strings.Contains(it.Value, "${{") {
				return nil, expr(it.Value)
			}
			out = append(out, it.Value)
		}
		if len(out) == 0 {
			return nil, "runs-on is an empty list"
		}
		return out, ""
	case yaml.MappingNode:
		return nil, "runs-on picks a runner group; spareci runners aren't in groups"
	case yaml.AliasNode:
		return nil, "runs-on is a YAML alias; edit the anchor by hand"
	}
	return nil, "runs-on has an unexpected form"
}

// rewritten is the new runs-on value. A list keeps every label in the
// fallback: a self-hosted runner has to carry all of them.
func rewritten(labels []string, varName string) string {
	if len(labels) == 1 {
		return fmt.Sprintf("${{ vars.%s || %s }}", varName, exprString(labels[0]))
	}
	js, _ := json.Marshal(labels)
	return fmt.Sprintf("${{ vars.%s || fromJSON(%s) }}", varName, exprString(string(js)))
}

// exprString quotes s as an Actions expression string literal.
func exprString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// makeEdit builds the line edit that sets runs-on (key k, value v) to expr.
func makeEdit(lines []string, k, v *yaml.Node, expr string) (*edit, error) {
	keyLine := k.Line - 1
	switch {
	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0:
		// Block list: put the expression after "runs-on:" and drop the items.
		line := []rune(lines[keyLine])
		colon := k.Column - 1 + len([]rune(sourceKey(lines, k)))
		for colon < len(line) && line[colon] == ' ' {
			colon++
		}
		if colon >= len(line) || line[colon] != ':' {
			return nil, fmt.Errorf("couldn't find runs-on's colon; edit it by hand")
		}
		rest := string(line[colon+1:])
		if t := strings.TrimSpace(rest); t != "" && !strings.HasPrefix(t, "#") {
			return nil, fmt.Errorf("unexpected text after runs-on:; edit it by hand")
		}
		first, last := v.Content[0].Line-1, v.Content[len(v.Content)-1].Line-1
		for i := first; i <= last; i++ {
			t := strings.TrimSpace(lines[i])
			if t != "" && !strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "#") {
				return nil, fmt.Errorf("runs-on list isn't one label per line; edit it by hand")
			}
		}
		newLine := string(line[:colon+1]) + " " + expr + strings.TrimRight(rest, " ")
		return &edit{start: keyLine, end: last + 1, lines: []string{newLine}}, nil
	case v.Kind == yaml.SequenceNode:
		for _, it := range v.Content {
			if it.Line != v.Line {
				return nil, fmt.Errorf("runs-on list spans lines; edit it by hand")
			}
		}
	}
	// Scalar or one-line [a, b]: replace the value's text on its line.
	idx := v.Line - 1
	line := []rune(lines[idx])
	start := v.Column - 1
	end, err := valueEnd(line, start, v)
	if err != nil {
		return nil, err
	}
	val := expr
	if v.Kind == yaml.ScalarNode && v.Style == yaml.DoubleQuotedStyle {
		val = `"` + expr + `"`
	}
	newLine := string(line[:start]) + val + string(line[end:])
	return &edit{start: idx, end: idx + 1, lines: []string{newLine}}, nil
}

// valueEnd finds where v's text, starting at line[start], ends.
func valueEnd(line []rune, start int, v *yaml.Node) (int, error) {
	bad := fmt.Errorf("couldn't find runs-on's value in the line; edit it by hand")
	if start >= len(line) {
		return 0, bad
	}
	switch {
	case v.Kind == yaml.SequenceNode:
		if line[start] != '[' {
			return 0, bad
		}
		for i := start; i < len(line); i++ {
			if line[i] == ']' {
				return i + 1, nil
			}
		}
	case v.Style == yaml.DoubleQuotedStyle:
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\\' {
				i++
			} else if line[i] == '"' {
				return i + 1, nil
			}
		}
	case v.Style == yaml.SingleQuotedStyle:
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, nil
			}
		}
	case v.Style == 0:
		if strings.HasPrefix(string(line[start:]), v.Value) {
			return start + len([]rune(v.Value)), nil
		}
	}
	return 0, bad
}

// sourceKey is the key as written (runs-on, maybe quoted).
func sourceKey(lines []string, k *yaml.Node) string {
	line := []rune(lines[k.Line-1])
	start := k.Column - 1
	if end, err := valueEnd(line, start, k); err == nil {
		return string(line[start:end])
	}
	return k.Value
}

// sourceText is the runs-on value as written, joined onto one line.
func sourceText(lines []string, k, v *yaml.Node) string {
	if v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 {
		return "[" + strings.Join(func() []string {
			var s []string
			for _, it := range v.Content {
				s = append(s, it.Value)
			}
			return s
		}(), ", ") + "]"
	}
	if v.Line < 1 || v.Line > len(lines) {
		return v.Value
	}
	line := []rune(lines[v.Line-1])
	if end, err := valueEnd(line, v.Column-1, v); err == nil {
		return string(line[v.Column-1 : end])
	}
	return v.Value
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	_, v := lookupPair(m, key)
	return v
}

func lookupPair(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// splitLines splits src into lines without their "\n" (a "\r" stays, so
// CRLF files round-trip).
func splitLines(src []byte) []string { return strings.Split(string(src), "\n") }

// Rewrite returns f's source with the given jobs converted. Every job must be
// from f and convertible.
func (f *File) Rewrite(jobs []*Job) ([]byte, error) {
	var edits []*edit
	for _, j := range jobs {
		if j.edit == nil {
			return nil, fmt.Errorf("%s: %s", j.Name(), j.Skip)
		}
		edits = append(edits, j.edit)
	}
	sort.Slice(edits, func(a, b int) bool { return edits[a].start > edits[b].start })
	lines := splitLines(f.Src)
	for _, e := range edits {
		lines = append(lines[:e.start], append(append([]string{}, e.lines...), lines[e.end:]...)...)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// Changed reports whether b differs from f's source.
func (f *File) Changed(b []byte) bool { return !bytes.Equal(f.Src, b) }
