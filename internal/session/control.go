package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
)

// Stop requests are files rather than OS signals because on Windows a signal
// terminates the target without running its shutdown path, which would skip
// the final checkpoint and leave the session looking live.

// stopPath returns the stop-request file for sessionID. The ID is validated
// because it becomes part of a file name.
func (s *Store) stopPath(sessionID string) (string, error) {
	if err := ValidateID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(s.home, "control", sessionID+".stop"), nil
}

// RequestStop asks the process broadcasting sessionID to shut down cleanly.
func (s *Store) RequestStop(sessionID string) error {
	path, err := s.stopPath(sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, strconv.AppendInt(nil, time.Now().UnixMilli(), 10), 0o600)
}

// ConsumeStop reports whether a stop was requested for sessionID, clearing
// the request if so. Broadcasts poll it.
func (s *Store) ConsumeStop(sessionID string) (bool, error) {
	path, err := s.stopPath(sessionID)
	if err != nil {
		return false, err
	}
	err = os.Remove(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// ClearStop removes any pending stop request for sessionID.
func (s *Store) ClearStop(sessionID string) error {
	_, err := s.ConsumeStop(sessionID)
	return err
}

const (
	idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	idLength   = 8
)

var idPattern = regexp.MustCompile(bigav1.PatternSessionID)

// ValidateID checks that a session or workspace ID is safe to use in URLs
// and file names.
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id %q: use 1-%d letters, digits, '-' or '_', starting with a letter or digit", id, bigav1.LimitMaxSessionIDLength)
	}
	return nil
}

// NewID returns a random 8-character base36 token. It is unguessable, which
// matters because the session ID is the share link.
func NewID() string {
	out := make([]byte, 0, idLength)
	buf := make([]byte, 16)
	for len(out) < idLength {
		rand.Read(buf)
		for _, b := range buf {
			// Rejecting bytes past the largest multiple of 36 keeps every
			// character equally likely.
			if b < 252 && len(out) < idLength {
				out = append(out, idAlphabet[b%36])
			}
		}
	}
	return string(out)
}

// NewWorkspaceID returns a random ID linking the sessions of one workspace.
func NewWorkspaceID() string {
	return "ws-" + NewID()
}
