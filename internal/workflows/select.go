package workflows

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Recommended returns the convertible jobs this machine suits.
func Recommended(files []*File) []*Job {
	var out []*Job
	for _, f := range files {
		for _, j := range f.Jobs {
			if j.Skip == "" && j.Fit == FitGood {
				out = append(out, j)
			}
		}
	}
	return out
}

// Convertible returns the jobs that can be chosen: not skipped, and not ones
// this machine can't run.
func Convertible(files []*File) []*Job {
	var out []*Job
	for _, f := range files {
		for _, j := range f.Jobs {
			if j.Skip == "" && j.Fit != FitNo {
				out = append(out, j)
			}
		}
	}
	return out
}

// Select resolves "workflow:job,..." to jobs. The workflow is the file name,
// with or without its extension.
func Select(files []*File, spec string) ([]*Job, error) {
	var out []*Job
	seen := map[*Job]bool{}
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		wf, id, ok := strings.Cut(name, ":")
		if !ok {
			return nil, fmt.Errorf("%q: want workflow:job, e.g. ci:build", name)
		}
		j := find(files, wf, id)
		if j == nil {
			return nil, fmt.Errorf("no job %q in %s", id, wf)
		}
		if j.Skip != "" {
			return nil, fmt.Errorf("%s: %s", j.Name(), j.Skip)
		}
		if j.Fit == FitNo {
			return nil, fmt.Errorf("%s: %s", j.Name(), j.Why)
		}
		if !seen[j] {
			seen[j] = true
			out = append(out, j)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no jobs named")
	}
	return out, nil
}

func find(files []*File, wf, id string) *Job {
	for _, f := range files {
		base := filepath.Base(f.Path)
		if wf != base && wf != strings.TrimSuffix(base, filepath.Ext(base)) {
			continue
		}
		for _, j := range f.Jobs {
			if j.ID == id {
				return j
			}
		}
	}
	return nil
}

// Change is a file's new content.
type Change struct {
	File *File
	New  []byte
}

// Apply rewrites each file that has chosen jobs.
func Apply(files []*File, chosen []*Job) ([]Change, error) {
	in := map[*Job]bool{}
	for _, j := range chosen {
		in[j] = true
	}
	var out []Change
	for _, f := range files {
		var jobs []*Job
		for _, j := range f.Jobs {
			if in[j] {
				jobs = append(jobs, j)
			}
		}
		if len(jobs) == 0 {
			continue
		}
		b, err := f.Rewrite(jobs)
		if err != nil {
			return nil, err
		}
		out = append(out, Change{File: f, New: b})
	}
	return out, nil
}
