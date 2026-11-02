package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errUnauthorized = errors.New("unauthorized")
	errConflict     = errors.New("conflict") //nolint:unused
	errNotFound     = errors.New("not found")
	errInvalid      = errors.New("invalid")
)

// WorkerConfigStore manages worker configurations
type WorkerConfigStore struct {
	dir string
	mu  sync.Mutex
}

// WorkerRecord represents a worker registration record
type WorkerRecord struct {
	WorkerID        string   `json:"worker_id"`
	WorkerTokenHash string   `json:"worker_token_hash"`
	Labels          []string `json:"labels,omitempty"`
	CreatedAtUnix   int64    `json:"created_at_unix,omitempty"`
	UpdatedAtUnix   int64    `json:"updated_at_unix,omitempty"`
}

type workerIndexFile struct {
	TokenHashToWorkerID map[string]string `json:"token_hash_to_worker_id"`
}

// NewWorkerConfigStore creates a new WorkerConfigStore
func NewWorkerConfigStore(dir string) (*WorkerConfigStore, error) {
	if dir == "" {
		dir = "./data"
	}
	dir = filepath.Clean(dir)
	s := &WorkerConfigStore{dir: dir}
	if err := s.ensureLayout(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *WorkerConfigStore) ensureLayout() error {
	if err := os.MkdirAll(filepath.Join(s.dir, "workers"), 0o755); err != nil {
		return err
	}
	if err := s.ensureJSONFile(filepath.Join(s.dir, "pairing_requests.json"), pairingRequestsFile{Requests: []PairingRequest{}}); err != nil {
		return err
	}
	if err := s.ensureJSONFile(filepath.Join(s.dir, "worker_index.json"), workerIndexFile{TokenHashToWorkerID: map[string]string{}}); err != nil {
		return err
	}

	// Cleanup old enroll_tokens.json if exists
	_ = os.Remove(filepath.Join(s.dir, "enroll_tokens.json"))
	return nil
}

func (s *WorkerConfigStore) ensureJSONFile(path string, v any) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeJSONAtomic(path, v)
}

// ListWorkers returns all workers
func (s *WorkerConfigStore) ListWorkers() ([]WorkerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(filepath.Join(s.dir, "workers"))
	if err != nil {
		return nil, err
	}
	out := make([]WorkerRecord, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		var rec WorkerRecord
		if err := readJSON(filepath.Join(s.dir, "workers", name), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAtUnix != out[j].UpdatedAtUnix {
			return out[i].UpdatedAtUnix > out[j].UpdatedAtUnix
		}
		return out[i].WorkerID < out[j].WorkerID
	})
	return out, nil
}

// RotateWorkerToken rotates a worker's token
func (s *WorkerConfigStore) RotateWorkerToken(workerID string) (string, *WorkerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerID == "" {
		return "", nil, errInvalid
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec WorkerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, errNotFound
		}
		return "", nil, err
	}
	newToken, err := generateToken()
	if err != nil {
		return "", nil, err
	}
	newHash := hashToken(newToken)

	var index workerIndexFile
	if err := readJSON(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return "", nil, err
	}
	if index.TokenHashToWorkerID == nil {
		index.TokenHashToWorkerID = map[string]string{}
	}
	if rec.WorkerTokenHash != "" {
		delete(index.TokenHashToWorkerID, rec.WorkerTokenHash)
	}
	index.TokenHashToWorkerID[newHash] = workerID
	if err := writeJSONAtomic(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return "", nil, err
	}

	rec.WorkerTokenHash = newHash
	rec.UpdatedAtUnix = time.Now().Unix()
	if err := writeJSONAtomic(workerPath, &rec); err != nil {
		return "", nil, err
	}
	return newToken, &rec, nil
}

// ValidateWorkerToken validates a worker token and returns the worker ID
func (s *WorkerConfigStore) ValidateWorkerToken(workerToken string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerToken == "" {
		return "", errUnauthorized
	}
	tokenHash := hashToken(workerToken)

	var index workerIndexFile
	if err := readJSON(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return "", err
	}
	workerID := index.TokenHashToWorkerID[tokenHash]
	if workerID == "" {
		return "", errUnauthorized
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec WorkerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errUnauthorized
		}
		return "", err
	}
	if rec.WorkerTokenHash != tokenHash {
		return "", errUnauthorized
	}
	return rec.WorkerID, nil
}

// DeleteWorker deletes a worker by ID
func (s *WorkerConfigStore) DeleteWorker(workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerID == "" {
		return errInvalid
	}

	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec WorkerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound
		}
		return err
	}

	// Remove from index
	var index workerIndexFile
	if err := readJSON(filepath.Join(s.dir, "worker_index.json"), &index); err == nil {
		if index.TokenHashToWorkerID != nil {
			if rec.WorkerTokenHash != "" {
				delete(index.TokenHashToWorkerID, rec.WorkerTokenHash)
				_ = writeJSONAtomic(filepath.Join(s.dir, "worker_index.json"), &index)
			}
		}
	}

	return os.Remove(workerPath)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
