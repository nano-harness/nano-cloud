package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type workerEnrollRequest struct {
	EnrollToken string   `json:"enroll_token"`
	WorkerID    string   `json:"worker_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

type workerEnrollResponse struct {
	WorkerID      string `json:"worker_id"`
	WorkerToken   string `json:"worker_token"`
	ConfigVersion string `json:"config_version"`
}

func (s *GatewayServer) handleWorkerEnroll(w http.ResponseWriter, r *http.Request) {
	var req workerEnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	workerID, workerToken, version, err := s.configStore.ConsumeEnrollToken(req.EnrollToken, req.WorkerID, req.Labels)
	if err != nil {
		switch {
		case errors.Is(err, errUnauthorized):
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		case errors.Is(err, errConflict):
			http.Error(w, "Conflict", http.StatusConflict)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(workerEnrollResponse{
		WorkerID:      workerID,
		WorkerToken:   workerToken,
		ConfigVersion: version,
	})
}

type workerGetConfigResponse struct {
	WorkerConfigYAML string `json:"worker_config_yaml"`
	AgentConfigYAML  string `json:"agent_config_yaml,omitempty"`
	ConfigVersion    string `json:"config_version"`
}

func (s *GatewayServer) handleWorkerGetConfig(w http.ResponseWriter, r *http.Request) {
	workerToken := parseBearerToken(r)
	rec, err := s.configStore.GetConfigByWorkerToken(workerToken)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	etag := `"` + rec.ConfigVersion + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	_ = json.NewEncoder(w).Encode(workerGetConfigResponse{
		WorkerConfigYAML: rec.WorkerConfigYAML,
		AgentConfigYAML:  rec.AgentConfigYAML,
		ConfigVersion:    rec.ConfigVersion,
	})
}

type workerConfigAckRequest struct {
	ConfigVersion string `json:"config_version"`
}

type workerConfigAckResponse struct {
	Accepted bool `json:"accepted"`
}

func (s *GatewayServer) handleWorkerConfigAck(w http.ResponseWriter, r *http.Request) {
	workerToken := parseBearerToken(r)
	var req workerConfigAckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := s.configStore.AckAppliedConfig(workerToken, req.ConfigVersion); err != nil {
		if errors.Is(err, errUnauthorized) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(workerConfigAckResponse{Accepted: true})
}

func parseBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.HasPrefix(auth, prefix) {
		return strings.TrimSpace(auth[len(prefix):])
	}
	return ""
}
