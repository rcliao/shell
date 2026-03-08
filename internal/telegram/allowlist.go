package telegram

import (
	"encoding/json"
	"os"
	"sync"
	"syscall"
	"time"
)

// AllowedUser represents a user approved via pairing.
type AllowedUser struct {
	UserID     int64  `json:"user_id"`
	Username   string `json:"username,omitempty"`
	FirstName  string `json:"first_name,omitempty"`
	ApprovedAt string `json:"approved_at"`
}

type allowlistData struct {
	Users []AllowedUser `json:"users"`
}

// AllowlistStore manages a JSON-file-backed allowlist with file locking.
type AllowlistStore struct {
	path string
	mu   sync.Mutex
}

// NewAllowlistStore creates a store at the given path.
func NewAllowlistStore(path string) *AllowlistStore {
	return &AllowlistStore{path: path}
}

// Load reads all approved users from the file.
func (s *AllowlistStore) Load() ([]AllowedUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Contains checks if a user ID exists in the allowlist.
func (s *AllowlistStore) Contains(userID int64) (bool, error) {
	users, err := s.Load()
	if err != nil {
		return false, err
	}
	for _, u := range users {
		if u.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

// Add appends a user to the allowlist (deduplicates by user ID).
func (s *AllowlistStore) Add(user AllowedUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		data = nil // treat read error as empty
	}

	// Deduplicate.
	for _, u := range data {
		if u.UserID == user.UserID {
			return nil // already exists
		}
	}

	data = append(data, user)
	return s.saveLocked(data)
}

// Remove deletes a user by ID from the allowlist.
func (s *AllowlistStore) Remove(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}

	filtered := data[:0]
	for _, u := range data {
		if u.UserID != userID {
			filtered = append(filtered, u)
		}
	}
	return s.saveLocked(filtered)
}

func (s *AllowlistStore) loadLocked() ([]AllowedUser, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var d allowlistData
	if err := json.NewDecoder(f).Decode(&d); err != nil {
		return nil, err
	}
	return d.Users, nil
}

func (s *AllowlistStore) saveLocked(users []AllowedUser) error {
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	d := allowlistData{Users: users}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// AllowlistPath returns the path to the allowlist file.
func (s *AllowlistStore) AllowlistPath() string {
	return s.path
}

// ListApproved returns all approved users (alias for Load, for CLI use).
func (s *AllowlistStore) ListApproved() ([]AllowedUser, error) {
	return s.Load()
}

// ApprovedAt returns the current time formatted for storage.
func ApprovedAt() string {
	return time.Now().UTC().Format(time.RFC3339)
}
