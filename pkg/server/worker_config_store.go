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

	"github.com/nano-harness/nano-cloud/pkg/worker"
	"github.com/google/uuid"
	"gopkg.in/yaml.v2"
)

var (
	errUnauthorized = errors.New("unauthorized")
	errConflict     = errors.New("conflict")
	errNotFound     = errors.New("not found")
	errInvalid      = errors.New("invalid")
)

type WorkerConfigStore struct {
	dir string
	mu  sync.Mutex
}

type enrollTokenRecord struct {
	Token         string `json:"token"`
	ExpiresAtUnix int64  `json:"expires_at_unix,omitempty"`
	Revoked       bool   `json:"revoked,omitempty"`
	UsedAtUnix    int64  `json:"used_at_unix,omitempty"`
	UsedBy        string `json:"used_by,omitempty"`
}

type enrollTokensFile struct {
	Tokens []enrollTokenRecord `json:"tokens"`
}

type workerRecord struct {
	WorkerID             string   `json:"worker_id"`
	WorkerTokenHash      string   `json:"worker_token_hash"`
	Labels               []string `json:"labels,omitempty"`
	ConfigVersion        string   `json:"config_version,omitempty"`
	WorkerConfigYAML     string   `json:"worker_config_yaml,omitempty"`
	AgentConfigYAML      string   `json:"agent_config_yaml,omitempty"`
	AppliedConfigVersion string   `json:"applied_config_version,omitempty"`
	CreatedAtUnix        int64    `json:"created_at_unix,omitempty"`
	UpdatedAtUnix        int64    `json:"updated_at_unix,omitempty"`
}

type workerIndexFile struct {
	TokenHashToWorkerID map[string]string `json:"token_hash_to_worker_id"`
}

type CreateEnrollTokenResult struct {
	Token         string `json:"token"`
	ExpiresAtUnix int64  `json:"expires_at_unix,omitempty"`
}

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
	if err := s.ensureJSONFile(filepath.Join(s.dir, "enroll_tokens.json"), enrollTokensFile{Tokens: []enrollTokenRecord{}}); err != nil {
		return err
	}
	if err := s.ensureJSONFile(filepath.Join(s.dir, "worker_index.json"), workerIndexFile{TokenHashToWorkerID: map[string]string{}}); err != nil {
		return err
	}
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

func (s *WorkerConfigStore) CreateEnrollToken(ttlSeconds int64) (CreateEnrollTokenResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := generateToken()
	if err != nil {
		return CreateEnrollTokenResult{}, err
	}
	now := time.Now().Unix()
	var exp int64
	if ttlSeconds > 0 {
		exp = now + ttlSeconds
	}

	var toks enrollTokensFile
	if err := readJSON(filepath.Join(s.dir, "enroll_tokens.json"), &toks); err != nil {
		return CreateEnrollTokenResult{}, err
	}
	toks.Tokens = append(toks.Tokens, enrollTokenRecord{
		Token:         token,
		ExpiresAtUnix: exp,
	})
	if err := writeJSONAtomic(filepath.Join(s.dir, "enroll_tokens.json"), &toks); err != nil {
		return CreateEnrollTokenResult{}, err
	}
	return CreateEnrollTokenResult{Token: token, ExpiresAtUnix: exp}, nil
}

func (s *WorkerConfigStore) ListEnrollTokens() ([]enrollTokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var toks enrollTokensFile
	if err := readJSON(filepath.Join(s.dir, "enroll_tokens.json"), &toks); err != nil {
		return nil, err
	}
	out := make([]enrollTokenRecord, 0, len(toks.Tokens))
	out = append(out, toks.Tokens...)
	return out, nil
}

func (s *WorkerConfigStore) RevokeEnrollToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if token == "" {
		return errInvalid
	}
	var toks enrollTokensFile
	if err := readJSON(filepath.Join(s.dir, "enroll_tokens.json"), &toks); err != nil {
		return err
	}
	found := false
	for i := range toks.Tokens {
		if toks.Tokens[i].Token == token {
			toks.Tokens[i].Revoked = true
			found = true
			break
		}
	}
	if !found {
		return errNotFound
	}
	return writeJSONAtomic(filepath.Join(s.dir, "enroll_tokens.json"), &toks)
}

func (s *WorkerConfigStore) ListWorkers() ([]workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(filepath.Join(s.dir, "workers"))
	if err != nil {
		return nil, err
	}
	out := make([]workerRecord, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		var rec workerRecord
		if err := readJSON(filepath.Join(s.dir, "workers", name), &rec); err != nil {
			continue
		}
		if _, err := s.ensureWorkerConfigLocked(&rec); err == nil {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAtUnix != out[j].UpdatedAtUnix {
			return out[i].UpdatedAtUnix > out[j].UpdatedAtUnix
		}
		return out[i].WorkerID < out[j].WorkerID
	})
	return out, nil
}

func (s *WorkerConfigStore) GetWorker(workerID string) (*workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerID == "" {
		return nil, errInvalid
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec workerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotFound
		}
		return nil, err
	}
	if _, err := s.ensureWorkerConfigLocked(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *WorkerConfigStore) UpdateWorkerConfig(workerID string, workerConfigYAML *string, agentConfigYAML *string) (*workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerID == "" {
		return nil, errInvalid
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec workerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotFound
		}
		return nil, err
	}
	changed := false

	if workerConfigYAML != nil {
		var cfg worker.Config
		if err := yaml.Unmarshal([]byte(*workerConfigYAML), &cfg); err != nil {
			return nil, errInvalid
		}
		if cfg.WorkerID != "" && cfg.WorkerID != workerID {
			return nil, errInvalid
		}
		rec.WorkerConfigYAML = *workerConfigYAML
		changed = true
	}
	if agentConfigYAML != nil {
		rec.AgentConfigYAML = *agentConfigYAML
		changed = true
	}

	if changed {
		rec.UpdatedAtUnix = time.Now().Unix()
		if _, err := s.ensureWorkerConfigLocked(&rec); err != nil {
			return nil, err
		}
		if err := writeJSONAtomic(workerPath, &rec); err != nil {
			return nil, err
		}
	}
	return &rec, nil
}

func (s *WorkerConfigStore) RotateWorkerToken(workerID string) (string, *workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerID == "" {
		return "", nil, errInvalid
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec workerRecord
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

func (s *WorkerConfigStore) ConsumeEnrollToken(enrollToken string, workerID string, labels []string) (newWorkerID string, workerToken string, configVersion string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if enrollToken == "" {
		return "", "", "", errUnauthorized
	}

	var toks enrollTokensFile
	if err := readJSON(filepath.Join(s.dir, "enroll_tokens.json"), &toks); err != nil {
		return "", "", "", err
	}

	now := time.Now().Unix()
	idx := -1
	for i := range toks.Tokens {
		if toks.Tokens[i].Token == enrollToken {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", "", "", errUnauthorized
	}
	t := toks.Tokens[idx]
	if t.Revoked {
		return "", "", "", errUnauthorized
	}
	if t.ExpiresAtUnix > 0 && now > t.ExpiresAtUnix {
		return "", "", "", errUnauthorized
	}
	if t.UsedAtUnix > 0 || t.UsedBy != "" {
		return "", "", "", errUnauthorized
	}

	if workerID == "" {
		workerID = uuid.NewString()
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	if _, err := os.Stat(workerPath); err == nil {
		return "", "", "", errConflict
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", "", err
	}

	workerToken, err = generateToken()
	if err != nil {
		return "", "", "", err
	}
	tokenHash := hashToken(workerToken)

	rec := &workerRecord{
		WorkerID:        workerID,
		WorkerTokenHash: tokenHash,
		Labels:          labels,
		CreatedAtUnix:   now,
		UpdatedAtUnix:   now,
	}
	if _, err := s.ensureWorkerConfigLocked(rec); err != nil {
		return "", "", "", err
	}

	toks.Tokens[idx].UsedAtUnix = now
	toks.Tokens[idx].UsedBy = workerID
	if err := writeJSONAtomic(filepath.Join(s.dir, "enroll_tokens.json"), &toks); err != nil {
		return "", "", "", err
	}

	var index workerIndexFile
	if err := readJSON(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return "", "", "", err
	}
	if index.TokenHashToWorkerID == nil {
		index.TokenHashToWorkerID = map[string]string{}
	}
	index.TokenHashToWorkerID[tokenHash] = workerID
	if err := writeJSONAtomic(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return "", "", "", err
	}

	if err := writeJSONAtomic(workerPath, rec); err != nil {
		return "", "", "", err
	}
	return rec.WorkerID, workerToken, rec.ConfigVersion, nil
}

func (s *WorkerConfigStore) GetConfigByWorkerToken(workerToken string) (*workerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerToken == "" {
		return nil, errUnauthorized
	}
	tokenHash := hashToken(workerToken)

	var index workerIndexFile
	if err := readJSON(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return nil, err
	}
	workerID := index.TokenHashToWorkerID[tokenHash]
	if workerID == "" {
		return nil, errUnauthorized
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec workerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errUnauthorized
		}
		return nil, err
	}
	if rec.WorkerTokenHash != tokenHash {
		return nil, errUnauthorized
	}

	changed, err := s.ensureWorkerConfigLocked(&rec)
	if err != nil {
		return nil, err
	}
	if changed {
		_ = writeJSONAtomic(workerPath, &rec)
	}
	return &rec, nil
}

func (s *WorkerConfigStore) AckAppliedConfig(workerToken string, appliedVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if workerToken == "" {
		return errUnauthorized
	}
	tokenHash := hashToken(workerToken)

	var index workerIndexFile
	if err := readJSON(filepath.Join(s.dir, "worker_index.json"), &index); err != nil {
		return err
	}
	workerID := index.TokenHashToWorkerID[tokenHash]
	if workerID == "" {
		return errUnauthorized
	}
	workerPath := filepath.Join(s.dir, "workers", workerID+".json")
	var rec workerRecord
	if err := readJSON(workerPath, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errUnauthorized
		}
		return err
	}
	if rec.WorkerTokenHash != tokenHash {
		return errUnauthorized
	}

	if appliedVersion == "" {
		appliedVersion = rec.ConfigVersion
	}
	rec.AppliedConfigVersion = appliedVersion
	rec.UpdatedAtUnix = time.Now().Unix()
	return writeJSONAtomic(workerPath, &rec)
}

func (s *WorkerConfigStore) ensureWorkerConfigLocked(rec *workerRecord) (bool, error) {
	now := time.Now().Unix()
	changed := false
	if rec.WorkerConfigYAML == "" {
		defaultCfg := worker.Config{
			RelayURL:       "",
			Token:          "",
			WorkerID:       rec.WorkerID,
			Name:           "nano-worker",
			Version:        "2.0",
			Labels:         rec.Labels,
			WorkspaceRoot:  "",
			EnvPassthrough: []string{"NANO_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
			Runtimes: map[string]worker.RuntimeConfig{
				"nano_agent": {
					Image: "nano-agent-runtime:local",
				},
				"claude_code": {
					Image: "nano-cli-runtime:local",
					Env:   map[string]string{"RUNTIME_BIN": "claude"},
				},
				"opencode": {
					Image: "nano-cli-runtime:local",
					Env:   map[string]string{"RUNTIME_BIN": "opencode"},
				},
				"custom": {
					Image: "nano-cli-runtime:local",
					Env:   map[string]string{"RUNTIME_BIN": "/bin/echo"},
				},
			},
		}
		b, err := yaml.Marshal(&defaultCfg)
		if err != nil {
			return false, err
		}
		rec.WorkerConfigYAML = string(b)
		rec.UpdatedAtUnix = now
		changed = true
	}

	version := computeConfigVersion(rec.WorkerConfigYAML, rec.AgentConfigYAML)
	if rec.ConfigVersion != version {
		rec.ConfigVersion = version
		rec.UpdatedAtUnix = now
		changed = true
	}
	return changed, nil
}

func computeConfigVersion(workerConfigYAML, agentConfigYAML string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(workerConfigYAML))
	_, _ = h.Write([]byte("\n---\n"))
	_, _ = h.Write([]byte(agentConfigYAML))
	return hex.EncodeToString(h.Sum(nil))
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
