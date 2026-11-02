package server

import (
	"crypto/rand"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

type PairingRequest struct { //nolint:revive
	ID          string   `json:"id"`
	UserCode    string   `json:"user_code"`   // 6-character short code for easier approval
	SecretHash  string   `json:"secret_hash"` // SHA256 of the secret held by worker
	WorkerName  string   `json:"worker_name"`
	HostInfo    string   `json:"host_info"`
	Labels      []string `json:"labels"`
	Status      string   `json:"status"` // pending, approved, rejected, expired
	CreatedAt   int64    `json:"created_at_unix"`
	ExpiresAt   int64    `json:"expires_at_unix"`
	WorkerToken string   `json:"worker_token,omitempty"` // Temporarily stored after approval
}

type pairingRequestsFile struct {
	Requests []PairingRequest `json:"requests"`
}

const (
	PairingStatusPending  = "pending" //nolint:revive
	PairingStatusApproved = "approved"
	PairingStatusRejected = "rejected"
	PairingStatusExpired  = "expired"
	PairingTTL            = 15 * time.Minute
)

func generateUserCode() string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // avoid confusing characters (O,0,I,1)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// CreatePairingRequest creates a new pairing request
func (s *WorkerConfigStore) CreatePairingRequest(workerName, hostInfo string, labels []string) (string, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	secret, err := generateToken()
	if err != nil {
		return "", "", "", err
	}
	secretHash := hashToken(secret)
	id := uuid.NewString()
	userCode := generateUserCode()

	now := time.Now().Unix()

	req := PairingRequest{
		ID:         id,
		UserCode:   userCode,
		SecretHash: secretHash,
		WorkerName: workerName,
		HostInfo:   hostInfo,
		Labels:     labels,
		Status:     PairingStatusPending,
		CreatedAt:  now,
		ExpiresAt:  now + int64(PairingTTL.Seconds()),
	}

	var file pairingRequestsFile
	// Ignore error if file doesn't exist, it will be created
	_ = readJSON(filepath.Join(s.dir, "pairing_requests.json"), &file)

	// Clean up expired
	valid := make([]PairingRequest, 0, len(file.Requests)+1)
	for _, r := range file.Requests {
		if r.ExpiresAt > now {
			valid = append(valid, r)
		}
	}
	valid = append(valid, req)
	file.Requests = valid

	if err := writeJSONAtomic(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
		return "", "", "", err
	}
	return id, secret, userCode, nil
}

// PollPairingRequest checks status. If approved, returns (approved, token).
// If token is returned, the request is removed or marked as consumed.
func (s *WorkerConfigStore) PollPairingRequest(id, secret string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var file pairingRequestsFile
	if err := readJSON(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
		return "", "", err
	}

	secretHash := hashToken(secret)
	now := time.Now().Unix()

	var found *PairingRequest
	idx := -1

	for i := range file.Requests {
		if file.Requests[i].ID == id {
			// Check secret
			if file.Requests[i].SecretHash != secretHash {
				return "", "", errUnauthorized
			}
			found = &file.Requests[i]
			idx = i
			break
		}
	}

	if found == nil {
		return "", "", errNotFound
	}
	if found.ExpiresAt < now {
		return PairingStatusExpired, "", nil
	}

	status := found.Status
	token := found.WorkerToken

	// If approved and token is present, consume it
	if status == PairingStatusApproved && token != "" {
		// Remove the request to prevent replay
		// We could also keep it as "completed" but removing is safer for "one-time token delivery"
		file.Requests = append(file.Requests[:idx], file.Requests[idx+1:]...)
		if err := writeJSONAtomic(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
			return "", "", err
		}
	} else if status == PairingStatusRejected { //nolint:revive,staticcheck
		// Also remove rejected to clean up? Or keep so client knows it's rejected?
		// Keep it until expiration or client stops polling.
	}

	return status, token, nil
}

// ApprovePairingRequest transitions a request to Approved and generates a Worker + Token
func (s *WorkerConfigStore) ApprovePairingRequest(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var file pairingRequestsFile
	if err := readJSON(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
		return err
	}

	now := time.Now().Unix()
	idx := -1
	for i := range file.Requests {
		if file.Requests[i].ID == id {
			idx = i
			break
		}
	}

	if idx == -1 {
		return errNotFound
	}
	if file.Requests[idx].ExpiresAt < now {
		return errors.New("request expired")
	}
	if file.Requests[idx].Status == PairingStatusApproved {
		return nil // Already approved
	}

	// Create Worker
	workerID := uuid.NewString()
	workerToken, err := generateToken()
	if err != nil {
		return err
	}
	tokenHash := hashToken(workerToken)

	// Create Worker Record
	rec := &WorkerRecord{
		WorkerID:        workerID,
		WorkerTokenHash: tokenHash,
		Labels:          file.Requests[idx].Labels,
		CreatedAtUnix:   now,
		UpdatedAtUnix:   now,
	}

	// Save Worker
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	if err := writeJSONAtomic(workerPath, rec); err != nil {
		return err
	}

	// Update Index
	var index workerIndexFile
	// It's possible index file doesn't exist yet
	_ = readJSON(filepath.Join(s.dir, "worker_index.json"), &index)
	if index.TokenHashToWorkerID == nil {
		index.TokenHashToWorkerID = map[string]string{}
	}
	index.TokenHashToWorkerID[tokenHash] = workerID
	if err := writeJSONAtomic(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return err
	}

	// Update Pairing Request
	file.Requests[idx].Status = PairingStatusApproved
	file.Requests[idx].WorkerToken = workerToken

	return writeJSONAtomic(filepath.Join(s.dir, "pairing_requests.json"), &file)
}

// ApprovePairingRequestByCode transitions a request to Approved by UserCode
func (s *WorkerConfigStore) ApprovePairingRequestByCode(userCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var file pairingRequestsFile
	if err := readJSON(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
		return err
	}

	now := time.Now().Unix()
	var found *PairingRequest
	idx := -1

	for i := range file.Requests {
		if file.Requests[i].UserCode == userCode {
			found = &file.Requests[i]
			idx = i
			break
		}
	}

	if found == nil {
		return errNotFound
	}
	if found.ExpiresAt < now {
		return errors.New("request expired")
	}
	if found.Status != PairingStatusPending {
		return errors.New("request not pending")
	}

	// Generate WorkerToken
	workerToken, err := generateToken()
	if err != nil {
		return err
	}
	tokenHash := hashToken(workerToken)

	// Create Worker Config
	workerID := uuid.NewString()
	rec := WorkerRecord{
		WorkerID:        workerID,
		WorkerTokenHash: tokenHash,
		Labels:          found.Labels,
		CreatedAtUnix:   now,
		UpdatedAtUnix:   now,
	}

	// Save Worker
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	if err := writeJSONAtomic(workerPath, &rec); err != nil {
		return err
	}

	// Update Index
	var index workerIndexFile
	_ = readJSON(filepath.Join(s.dir, "worker_index.json"), &index)
	if index.TokenHashToWorkerID == nil {
		index.TokenHashToWorkerID = map[string]string{}
	}
	index.TokenHashToWorkerID[tokenHash] = workerID
	if err := writeJSONAtomic(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return err
	}

	// Update Pairing Request
	file.Requests[idx].Status = PairingStatusApproved
	file.Requests[idx].WorkerToken = workerToken

	return writeJSONAtomic(filepath.Join(s.dir, "pairing_requests.json"), &file)
}

// RejectPairingRequest rejects a request
func (s *WorkerConfigStore) RejectPairingRequest(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var file pairingRequestsFile
	if err := readJSON(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
		return err
	}

	idx := -1
	for i := range file.Requests {
		if file.Requests[i].ID == id {
			idx = i
			break
		}
	}

	if idx == -1 {
		return errNotFound
	}

	file.Requests[idx].Status = PairingStatusRejected
	return writeJSONAtomic(filepath.Join(s.dir, "pairing_requests.json"), &file)
}

// ListPairingRequests returns all pending requests
func (s *WorkerConfigStore) ListPairingRequests() ([]PairingRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var file pairingRequestsFile
	if err := readJSON(filepath.Join(s.dir, "pairing_requests.json"), &file); err != nil {
		return []PairingRequest{}, nil
	}

	now := time.Now().Unix()
	out := make([]PairingRequest, 0)
	for _, r := range file.Requests {
		if r.ExpiresAt > now {
			// Don't leak token or secret hash in list
			r.WorkerToken = ""
			r.SecretHash = ""
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}
