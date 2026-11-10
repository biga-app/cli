// Package session keeps the local registry of broadcast sessions under the
// biga home directory. The registry maps each broadcast directory to its
// session, which is what keeps a project's share URL stable across restarts.
package session

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// LivenessWindow is how recently a broadcasting process must have refreshed
// its record for the session to count as live.
const LivenessWindow = 30 * time.Second

// Record describes one broadcast directory's session.
type Record struct {
	SessionID         string    `json:"sessionId"`
	Title             string    `json:"title"`
	RootDir           string    `json:"rootDir"`
	StartedAt         time.Time `json:"startedAt"`
	APIURL            string    `json:"apiBaseUrl"`
	CheckpointsSynced int64     `json:"checkpointsSynced"`
	// Token is the session's owner token: the only proof this machine may
	// write to the session. The registry file is readable by its owner only.
	Token string `json:"token,omitzero"`
	// ShareLink is the viewer link the server reported, if it knows its
	// public address.
	ShareLink string `json:"shareUrl,omitzero"`
	// Key is the secret an encrypted session's content is sealed with. The
	// server never sees it; it travels in the share link.
	Key string `json:"key,omitzero"`
	// PID and LastSeenAt identify the process broadcasting the session. Both
	// are cleared when the broadcast stops.
	PID        int       `json:"pid,omitzero"`
	LastSeenAt time.Time `json:"lastSeenAt,omitzero"`
}

// ShareURL is the viewer URL for the session.
func (r Record) ShareURL() string {
	link := r.ShareLink
	if link == "" {
		link = ShareURL(r.APIURL, r.SessionID)
	}
	return WithKey(link, r.Key)
}

// WithKey puts an encrypted session's key in the link's fragment, which
// browsers keep to themselves. An empty key leaves the link as it is.
func WithKey(link, key string) string {
	if key == "" {
		return link
	}
	return link + "#k=" + key
}

// ShareURL is the viewer URL for a session on the given server.
func ShareURL(apiURL, sessionID string) string {
	return strings.TrimRight(apiURL, "/") + "/session/" + sessionID
}

// Live reports whether a process is still broadcasting the session. It needs
// both a running PID and a recent refresh: the PID catches a process that
// exited, and the timestamp catches a PID since reused by something else.
func (r Record) Live(now time.Time) bool {
	if r.PID <= 0 || r.LastSeenAt.IsZero() || now.Sub(r.LastSeenAt) >= LivenessWindow {
		return false
	}
	return processAlive(r.PID)
}

// Store reads and writes the registry in a home directory. Writes are
// serialized across processes with a lock, and replace the file atomically.
type Store struct {
	home string
}

// NewStore returns a store rooted at home, normally ~/.biga.
func NewStore(home string) *Store {
	return &Store{home: home}
}

func (s *Store) registryPath() string { return filepath.Join(s.home, "sessions.json") }

// Lookup returns the record for rootDir, if there is one.
func (s *Store) Lookup(rootDir string) (Record, bool, error) {
	key, err := dirKey(rootDir)
	if err != nil {
		return Record{}, false, err
	}
	registry, err := s.read()
	if err != nil {
		return Record{}, false, err
	}
	r, ok := registry[key]
	return r, ok, nil
}

// List returns every record, sorted by directory.
func (s *Store) List() ([]Record, error) {
	registry, err := s.read()
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(registry))
	for _, r := range registry {
		records = append(records, r)
	}
	slices.SortFunc(records, func(a, b Record) int { return cmp.Compare(a.RootDir, b.RootDir) })
	return records, nil
}

// Save inserts or replaces the record for r.RootDir.
func (s *Store) Save(r Record) error {
	key, err := dirKey(r.RootDir)
	if err != nil {
		return err
	}
	r.RootDir = key
	return s.update(func(registry map[string]Record) bool {
		registry[key] = r
		return true
	})
}

// Remove deletes the record for rootDir. It is a no-op when there is none.
func (s *Store) Remove(rootDir string) error {
	key, err := dirKey(rootDir)
	if err != nil {
		return err
	}
	return s.update(func(registry map[string]Record) bool {
		if _, ok := registry[key]; !ok {
			return false
		}
		delete(registry, key)
		return true
	})
}

// update applies fn to the registry under the lock and writes the result
// when fn reports a change.
func (s *Store) update(fn func(map[string]Record) bool) error {
	return s.withLock(func() error {
		registry, err := s.read()
		if err != nil {
			return err
		}
		if !fn(registry) {
			return nil
		}
		return s.write(registry)
	})
}

func (s *Store) read() (map[string]Record, error) {
	data, err := os.ReadFile(s.registryPath())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session registry: %w", err)
	}
	registry := map[string]Record{}
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("session registry %s is corrupt (delete it to start fresh): %w", s.registryPath(), err)
	}
	return registry, nil
}

// write replaces the registry atomically: a reader never sees a half-written
// file, and a crash mid-write leaves the previous version intact.
func (s *Store) write(registry map[string]Record) error {
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.registryPath(), append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// dirKey normalizes a directory into the registry key.
func dirKey(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	return abs, nil
}
