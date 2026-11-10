package session

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// staleLockAge is how old a lock may get before it is treated as left
	// behind by a crashed process. Registry writes take milliseconds.
	staleLockAge = 10 * time.Second
	// lockTimeout bounds how long a process waits for the lock.
	lockTimeout = 3 * time.Second
)

// contended reports whether a failed Mkdir means another process holds the
// lock. Windows answers "access denied" instead of "exists" while a lock
// directory that was just removed is still being deleted.
func contended(err error) bool {
	return errors.Is(err, fs.ErrExist) || (runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission))
}

func (s *Store) lockPath() string { return filepath.Join(s.home, "sessions.lock") }

// withLock serializes read-modify-write cycles on the registry across
// processes, e.g. `biga start` running in two terminals. Without it both read
// the same registry and the second write discards the first one's record.
//
// The lock is a directory because creating one is atomic on every platform:
// exactly one concurrent caller succeeds.
func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return err
	}
	lock := s.lockPath()
	deadline := time.Now().Add(lockTimeout)
	for {
		err := os.Mkdir(lock, 0o700)
		if err == nil {
			break
		}
		if !contended(err) {
			return fmt.Errorf("acquire registry lock: %w", err)
		}
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > staleLockAge {
			// If another waiter removes it first, this fails harmlessly.
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for the session registry lock (%w); if no other biga process is running, delete %s and retry", err, lock)
		}
		time.Sleep(50*time.Millisecond + rand.N(50*time.Millisecond))
	}
	defer os.Remove(lock)
	return fn()
}
