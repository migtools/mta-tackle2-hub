package record

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const metaFile = "meta.json"

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// safe reports whether v is usable as a single path element component.
func safe(v string) bool {
	return v != "" && !strings.ContainsAny(v, `/\`) && !strings.Contains(v, "..")
}

func validate(name, uid string) error {
	if !safe(name) || !safe(uid) {
		return errors.New("invalid run name or uid")
	}
	return nil
}

// Gap is a period during which recording was interrupted.
type Gap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Meta describes the recording state of an agent run.
type Meta struct {
	Run        string     `json:"run"`
	UID        string     `json:"uid"`
	Transcript string     `json:"transcript"` // recording|complete|incomplete|skipped
	SkipReason string     `json:"skipReason,omitempty"`
	Gaps       []Gap      `json:"gaps,omitempty"`
	Started    *time.Time `json:"started,omitempty"`
	Ended      *time.Time `json:"ended,omitempty"`
}

// Store is a directory-per-run recording store.
// Root is <bucket>/agentruns.
type Store struct {
	Root string
}

// Dir returns the directory for the run.
func (s *Store) Dir(name, uid string) string {
	return filepath.Join(s.Root, name+"-"+uid)
}

// ReadMeta reads the run metadata. Returns false when absent.
func (s *Store) ReadMeta(name, uid string) (m Meta, found bool, err error) {
	err = validate(name, uid)
	if err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(name, uid), metaFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return
	}
	err = json.Unmarshal(b, &m)
	if err != nil {
		return
	}
	found = true
	return
}

// WriteMeta atomically writes the run metadata (temp file + rename).
func (s *Store) WriteMeta(name, uid string, m Meta) (err error) {
	err = validate(name, uid)
	if err != nil {
		return
	}
	dir := s.Dir(name, uid)
	err = os.MkdirAll(dir, 0o755)
	if err != nil {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, metaFile+".*.tmp")
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, err = tmp.Write(b)
	if err != nil {
		_ = tmp.Close()
		return
	}
	err = tmp.Close()
	if err != nil {
		return
	}
	err = os.Rename(tmp.Name(), filepath.Join(dir, metaFile))
	return
}

// OpenAppend opens (creating as needed) file for append and returns its current size.
func (s *Store) OpenAppend(name, uid, file string) (f *os.File, size int64, err error) {
	err = validate(name, uid)
	if err == nil && !safe(file) {
		err = errors.New("invalid file name")
	}
	if err != nil {
		return
	}
	dir := s.Dir(name, uid)
	err = os.MkdirAll(dir, 0o755)
	if err != nil {
		return
	}
	f, err = os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		f = nil
		return
	}
	size = st.Size()
	return
}

// Find returns the newest directory named <name>-<uuid>.
func (s *Store) Find(name string) (dir string, uid string, found bool) {
	if !safe(name) {
		return
	}
	ents, err := os.ReadDir(s.Root)
	if err != nil {
		return
	}
	prefix := name + "-"
	var newest time.Time
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) ||
			!uuidRe.MatchString(strings.TrimPrefix(e.Name(), prefix)) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !found || info.ModTime().After(newest) {
			newest = info.ModTime()
			dir = filepath.Join(s.Root, e.Name())
			uid = strings.TrimPrefix(e.Name(), prefix)
			found = true
		}
	}
	return
}
