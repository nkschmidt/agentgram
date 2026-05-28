package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
)

// fileName — settings file name in the bot's working directory.
// The path isn't configurable from the outside: settings always sit next
// to where the binary is launched, which simplifies deployment and
// promises uniform behavior locally and on the server.
const fileName = "settings.json"

// Store — thread-safe settings repository on top of a JSON file.
// Serialization is protected by sync.RWMutex; readers don't block each other.
//
// onWorkDirChange (if set via SetOnWorkDirChange) is called AFTER
// successful save of a new WorkDir value for a particular user, and only
// if the value actually changed. The callback runs in a separate goroutine,
// so long-running logic (restarting backend processes) doesn't block the write.
type Store struct {
	mu              sync.RWMutex
	data            Settings
	onWorkDirChange func(userID int64)
}

// Open reads settings from the file in the working directory. If the file
// doesn't exist — creates it with defaults. A corrupted file is an error:
// we don't silently "heal" it, so as not to lose data.
func Open() (*Store, error) {
	s := &Store{data: defaults()}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(fileName)
	if errors.Is(err, os.ErrNotExist) {
		return s.persist()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return fmt.Errorf("parse %s: %w", fileName, err)
	}
	s.normalize()
	return nil
}

// normalize ensures nil collections become empty — calling code can work
// with them without nil checks.
func (s *Store) normalize() {
	if s.data.AllowedUsers == nil {
		s.data.AllowedUsers = []int64{}
	}
	if s.data.WorkDirs == nil {
		s.data.WorkDirs = map[int64]string{}
	}
}

func (s *Store) persist() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(fileName, b, 0o600)
}

// IsAllowed returns true if the user is in the whitelist.
func (s *Store) IsAllowed(userID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Contains(s.data.AllowedUsers, userID)
}

// AllowFirstIfEmpty — atomic operation: if the whitelist is empty, adds
// userID and saves the file. Returns added=true if the user was added.
// If the list is not empty — added=false, err=nil.
func (s *Store) AllowFirstIfEmpty(userID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.AllowedUsers) != 0 {
		return false, nil
	}
	s.data.AllowedUsers = append(s.data.AllowedUsers, userID)
	if err := s.persist(); err != nil {
		s.data.AllowedUsers = s.data.AllowedUsers[:0]
		return false, err
	}
	return true, nil
}

// ListAllowed returns a copy of the current whitelist. A copy — so the caller
// can't accidentally mutate internal state without holding the mutex.
func (s *Store) ListAllowed() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]int64, len(s.data.AllowedUsers))
	copy(out, s.data.AllowedUsers)
	return out
}

// AllowUser idempotently adds a user to the whitelist.
// added=true means "was added on this call".
func (s *Store) AllowUser(userID int64) (added bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.data.AllowedUsers, userID) {
		return false, nil
	}
	s.data.AllowedUsers = append(s.data.AllowedUsers, userID)
	if err := s.persist(); err != nil {
		s.data.AllowedUsers = s.data.AllowedUsers[:len(s.data.AllowedUsers)-1]
		return false, err
	}
	return true, nil
}

// WorkDirOf returns a user's working directory (or "" if not set).
func (s *Store) WorkDirOf(userID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.WorkDirs[userID]
}

// SetWorkDirOf saves the user's working directory. Empty string = "reset"
// (use the bot's cwd). Validation (does the path exist, is it a directory) —
// caller's concern. On an actual change triggers onWorkDirChange(userID).
func (s *Store) SetWorkDirOf(userID int64, path string) error {
	s.mu.Lock()
	prev := s.data.WorkDirs[userID]
	if prev == path {
		s.mu.Unlock()
		return nil
	}
	if path == "" {
		delete(s.data.WorkDirs, userID)
	} else {
		s.data.WorkDirs[userID] = path
	}
	if err := s.persist(); err != nil {
		// in-memory rollback
		if prev == "" {
			delete(s.data.WorkDirs, userID)
		} else {
			s.data.WorkDirs[userID] = prev
		}
		s.mu.Unlock()
		return err
	}
	cb := s.onWorkDirChange
	s.mu.Unlock()

	if cb != nil {
		go cb(userID)
	}
	return nil
}

// SetOnWorkDirChange registers a callback that's called on every actual
// change of a user's WorkDir (after persist). Used in main to restart
// that user's active session in the new folder.
func (s *Store) SetOnWorkDirChange(fn func(userID int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onWorkDirChange = fn
}

// WhisperBin / WhisperModel — global whisper.cpp settings.
// The transcriber reads them on every request (via provider functions),
// so a change in /settings applies immediately.

func (s *Store) WhisperBin() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.WhisperBin
}

func (s *Store) SetWhisperBin(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.WhisperBin == path {
		return nil
	}
	prev := s.data.WhisperBin
	s.data.WhisperBin = path
	if err := s.persist(); err != nil {
		s.data.WhisperBin = prev
		return err
	}
	return nil
}

func (s *Store) WhisperModel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.WhisperModel
}

func (s *Store) SetWhisperModel(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.WhisperModel == path {
		return nil
	}
	prev := s.data.WhisperModel
	s.data.WhisperModel = path
	if err := s.persist(); err != nil {
		s.data.WhisperModel = prev
		return err
	}
	return nil
}

// RevokeUser idempotently removes a user from the whitelist.
// removed=true means "was removed on this call".
func (s *Store) RevokeUser(userID int64) (removed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.Index(s.data.AllowedUsers, userID)
	if idx < 0 {
		return false, nil
	}
	backup := slices.Clone(s.data.AllowedUsers)
	s.data.AllowedUsers = slices.Delete(s.data.AllowedUsers, idx, idx+1)
	if err := s.persist(); err != nil {
		s.data.AllowedUsers = backup
		return false, err
	}
	return true, nil
}
