package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is one download in the list (finished, paused or failed).
type Entry struct {
	ID      int64     `json:"id"`
	Kind    string    `json:"kind"` // "file", "torrent" or "media"
	URL     string    `json:"url"`
	Format  string    `json:"format,omitempty"` // media: the quality that was picked
	Name    string    `json:"name"`
	Path    string    `json:"path"` // the finished file (or folder, for torrents)
	Dir     string    `json:"dir"`
	Size    int64     `json:"size"`
	Done    int64     `json:"done"`
	Status  string    `json:"status"`          // "downloading", "paused", "done", "failed"
	Extra   []string  `json:"extra,omitempty"` // other files to delete along with it
	Added   time.Time `json:"added"`
	Updated time.Time `json:"updated"`
}

type Store struct {
	path    string
	Entries []*Entry `json:"entries"`
	NextID  int64    `json:"next_id"`
}

func storePath() string {
	if h := os.Getenv("SWOOP_HOME"); h != "" {
		return filepath.Join(h, "downloads.json")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "swoop", "downloads.json")
}

func loadStore() *Store {
	s := &Store{path: storePath(), NextID: 1}
	if s.path == "" {
		return s
	}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, s)
	}
	if s.NextID < 1 {
		s.NextID = 1
	}
	return s
}

func (s *Store) save() {
	if s.path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.path)
	}
}

func (s *Store) find(kind, url, format string) *Entry {
	for _, e := range s.Entries {
		if e.Kind == kind && e.URL == url && e.Format == format {
			return e
		}
	}
	return nil
}

// track records a download that is starting (or starting again).
func (s *Store) track(e *Entry) *Entry {
	now := time.Now()
	if old := s.find(e.Kind, e.URL, e.Format); old != nil {
		old.Name, old.Path, old.Dir, old.Size = e.Name, e.Path, e.Dir, e.Size
		if len(e.Extra) > 0 {
			old.Extra = e.Extra
		}
		old.Status, old.Updated = "downloading", now
		s.save()
		return old
	}
	e.ID = s.NextID
	s.NextID++
	e.Status, e.Added, e.Updated = "downloading", now, now
	s.Entries = append(s.Entries, e)
	s.save()
	return e
}

func (s *Store) finish(e *Entry, status string, done int64) {
	e.Status, e.Done, e.Updated = status, done, time.Now()
	s.save()
}

func (s *Store) remove(e *Entry) {
	for i, x := range s.Entries {
		if x == e {
			s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
			break
		}
	}
	s.save()
}

// unfinished counts downloads that can be continued.
func (s *Store) unfinished() int {
	n := 0
	for _, e := range s.Entries {
		if e.Status != "done" {
			n++
		}
	}
	return n
}

// deleteFiles removes everything on disk that belongs to a download. Every
// path must sit inside the download's own folder, so a broken or edited
// list can never delete anything else.
func (e *Entry) deleteFiles() {
	paths := append([]string{}, e.Extra...)
	if e.Path != "" {
		paths = append(paths, e.Path, e.Path+".part", e.Path+".part.json", e.Path+".aria2")
	}
	for _, p := range paths {
		if insideDir(e.Dir, p) {
			os.RemoveAll(p)
		}
	}
}

func insideDir(dir, p string) bool {
	if dir == "" || p == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
