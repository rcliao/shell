package telegram

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// PairingRequest represents a pending pairing code for an unknown sender.
type PairingRequest struct {
	Code      string `json:"code"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	ChatID    int64  `json:"chat_id"`
	CreatedAt string `json:"created_at"`
}

type pairingFileData struct {
	Requests []*PairingRequest `json:"requests"`
}

// PairingManager manages pending pairing requests with code generation and expiry.
// Pending requests are persisted to a JSON file for cross-process access (CLI approve).
type PairingManager struct {
	mu       sync.Mutex
	byCode   map[string]*PairingRequest // code → request
	byUser   map[int64]string           // userID → code
	store    *AllowlistStore
	filePath string // ~/.shell/pairing.json
	ttl      time.Duration
	maxPending int
}

// NewPairingManager creates a pairing manager backed by the given allowlist store.
func NewPairingManager(store *AllowlistStore, pairingPath string, ttl time.Duration) *PairingManager {
	pm := &PairingManager{
		byCode:     make(map[string]*PairingRequest),
		byUser:     make(map[int64]string),
		store:      store,
		filePath:   pairingPath,
		ttl:        ttl,
		maxPending: 20,
	}
	// Load existing pending requests from disk.
	pm.loadFromDisk()
	return pm
}

// RequestPairing generates or returns an existing 8-char code for an unknown sender.
func (pm *PairingManager) RequestPairing(userID int64, username, firstName, lastName string, chatID int64) (string, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.cleanupLocked()

	// Return existing code if user already has a pending request.
	if code, ok := pm.byUser[userID]; ok {
		return code, nil
	}

	// Check max pending limit.
	if len(pm.byCode) >= pm.maxPending {
		return "", fmt.Errorf("too many pending pairing requests")
	}

	code, err := generateCode()
	if err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}

	req := &PairingRequest{
		Code:      code,
		UserID:    userID,
		Username:  username,
		FirstName: firstName,
		LastName:  lastName,
		ChatID:    chatID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	pm.byCode[code] = req
	pm.byUser[userID] = code
	pm.saveToDisk()
	return code, nil
}

// Approve finds a pending request by code, adds the user to the allowlist, and removes the request.
func (pm *PairingManager) Approve(code string) (*PairingRequest, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.cleanupLocked()

	req, ok := pm.byCode[code]
	if !ok {
		return nil, fmt.Errorf("pairing code not found or expired: %s", code)
	}

	// Add to persistent allowlist.
	if err := pm.store.Add(AllowedUser{
		UserID:     req.UserID,
		Username:   req.Username,
		FirstName:  req.FirstName,
		ApprovedAt: ApprovedAt(),
	}); err != nil {
		return nil, fmt.Errorf("add to allowlist: %w", err)
	}

	// Remove from pending.
	delete(pm.byCode, code)
	delete(pm.byUser, req.UserID)
	pm.saveToDisk()

	return req, nil
}

// List returns all non-expired pending pairing requests.
func (pm *PairingManager) List() []*PairingRequest {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.cleanupLocked()

	result := make([]*PairingRequest, 0, len(pm.byCode))
	for _, req := range pm.byCode {
		result = append(result, req)
	}
	return result
}

// Cleanup removes expired requests.
func (pm *PairingManager) Cleanup() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.cleanupLocked()
}

func (pm *PairingManager) cleanupLocked() {
	cutoff := time.Now().Add(-pm.ttl)
	for code, req := range pm.byCode {
		created, err := time.Parse(time.RFC3339, req.CreatedAt)
		if err != nil || created.Before(cutoff) {
			delete(pm.byCode, code)
			delete(pm.byUser, req.UserID)
		}
	}
}

func (pm *PairingManager) loadFromDisk() {
	f, err := os.Open(pm.filePath)
	if err != nil {
		return
	}
	defer f.Close()

	syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var data pairingFileData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return
	}

	cutoff := time.Now().Add(-pm.ttl)
	for _, req := range data.Requests {
		created, err := time.Parse(time.RFC3339, req.CreatedAt)
		if err != nil || created.Before(cutoff) {
			continue
		}
		pm.byCode[req.Code] = req
		pm.byUser[req.UserID] = req.Code
	}
}

func (pm *PairingManager) saveToDisk() {
	requests := make([]*PairingRequest, 0, len(pm.byCode))
	for _, req := range pm.byCode {
		requests = append(requests, req)
	}

	f, err := os.OpenFile(pm.filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data := pairingFileData{Requests: requests}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(data)
}

// LoadPendingFromFile reads pending requests directly from the pairing file.
// Used by CLI commands that run out-of-process.
func LoadPendingFromFile(path string) ([]*PairingRequest, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var data pairingFileData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, err
	}
	return data.Requests, nil
}

// ApproveFromFile approves a pairing code from the file (CLI use, out-of-process).
// Returns the approved request.
func ApproveFromFile(pairingPath string, allowlistStore *AllowlistStore, code string) (*PairingRequest, error) {
	requests, err := LoadPendingFromFile(pairingPath)
	if err != nil {
		return nil, err
	}

	var found *PairingRequest
	remaining := make([]*PairingRequest, 0, len(requests))
	for _, req := range requests {
		if req.Code == code {
			found = req
		} else {
			remaining = append(remaining, req)
		}
	}
	if found == nil {
		return nil, fmt.Errorf("pairing code not found: %s", code)
	}

	// Add to allowlist.
	if err := allowlistStore.Add(AllowedUser{
		UserID:     found.UserID,
		Username:   found.Username,
		FirstName:  found.FirstName,
		ApprovedAt: ApprovedAt(),
	}); err != nil {
		return nil, fmt.Errorf("add to allowlist: %w", err)
	}

	// Save remaining requests.
	f, err := os.OpenFile(pairingPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return found, nil // approved but couldn't clean up file
	}
	defer f.Close()
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(pairingFileData{Requests: remaining})

	return found, nil
}

// generateCode creates a cryptographically random 8-character code.
// Uses alphabet without ambiguous characters (no 0/O/1/I/l).
func generateCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := make([]byte, 8)
	for i := range code {
		code[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(code), nil
}
