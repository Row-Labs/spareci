package stats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// PendingFile holds a repo's finished jobs whose record isn't written yet,
// because GitHub hadn't caught up when spareci stopped (or crashed). The next
// `spareci up` for the repo resumes them. One file per repo, since each repo
// has its own spareci process.
func (r *Recorder) PendingFile() string {
	return filepath.Join(filepath.Dir(r.Path), "pending", strings.ReplaceAll(r.Repo, "/", "_")+".json")
}

// Pending is how many finished jobs aren't recorded yet.
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.unsaved)
}

func (r *Recorder) addPending(f Finished) error {
	r.mu.Lock()
	if r.unsaved == nil {
		r.unsaved = map[string]Finished{}
	}
	r.unsaved[f.Runner] = f
	r.mu.Unlock()
	return r.savePending()
}

func (r *Recorder) removePending(runner string) error {
	r.mu.Lock()
	delete(r.unsaved, runner)
	r.mu.Unlock()
	return r.savePending()
}

// savePendingList replaces the pending set with list.
func (r *Recorder) savePendingList(list []Finished) error {
	r.mu.Lock()
	r.unsaved = map[string]Finished{}
	for _, f := range list {
		r.unsaved[f.Runner] = f
	}
	r.mu.Unlock()
	return r.savePending()
}

// savePending writes the pending set, or removes the file when it's empty.
func (r *Recorder) savePending() error {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	r.mu.Lock()
	list := make([]Finished, 0, len(r.unsaved))
	for _, f := range r.unsaved {
		list = append(list, f)
	}
	r.mu.Unlock()
	slices.SortFunc(list, func(a, b Finished) int { return a.Ended.Compare(b.Ended) })

	path := r.PendingFile()
	if len(list) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadPending reads the pending file. A missing file is nothing pending.
func (r *Recorder) loadPending() ([]Finished, error) {
	b, err := os.ReadFile(r.PendingFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Finished
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	// Drop entries that can't be a job (a hand-edited file, say).
	return slices.DeleteFunc(list, func(f Finished) bool { return f.Runner == "" || f.Ended.Equal(time.Time{}) }), nil
}
