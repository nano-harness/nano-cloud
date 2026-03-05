package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/mux"
)

func (s *GatewayServer) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.token == "" {
		http.Error(w, "Gateway token is required", http.StatusServiceUnavailable)
		return false
	}
	if parseBearerToken(r) != s.token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

type adminCreateEnrollTokenRequest struct {
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

func (s *GatewayServer) handleAdminCreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req adminCreateEnrollTokenRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	out, err := s.configStore.CreateEnrollToken(req.TTLSeconds)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *GatewayServer) handleAdminListEnrollTokens(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	toks, err := s.configStore.ListEnrollTokens()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
}

func (s *GatewayServer) handleAdminRevokeEnrollToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	token := mux.Vars(r)["token"]
	if err := s.configStore.RevokeEnrollToken(token); err != nil {
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		case errors.Is(err, errInvalid):
			http.Error(w, "Bad Request", http.StatusBadRequest)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"revoked": true})
}

func (s *GatewayServer) handleAdminListWorkers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list, err := s.configStore.ListWorkers()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, rec := range list {
		out = append(out, map[string]any{
			"worker_id":              rec.WorkerID,
			"labels":                 rec.Labels,
			"config_version":         rec.ConfigVersion,
			"applied_config_version": rec.AppliedConfigVersion,
			"created_at_unix":        rec.CreatedAtUnix,
			"updated_at_unix":        rec.UpdatedAtUnix,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *GatewayServer) handleAdminGetWorkerConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	workerID := mux.Vars(r)["id"]
	rec, err := s.configStore.GetWorker(workerID)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		case errors.Is(err, errInvalid):
			http.Error(w, "Bad Request", http.StatusBadRequest)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"worker_id":              rec.WorkerID,
		"worker_config_yaml":     rec.WorkerConfigYAML,
		"agent_config_yaml":      rec.AgentConfigYAML,
		"config_version":         rec.ConfigVersion,
		"applied_config_version": rec.AppliedConfigVersion,
	})
}

type adminPutWorkerConfigRequest struct {
	WorkerConfigYAML *string `json:"worker_config_yaml"`
	AgentConfigYAML  *string `json:"agent_config_yaml"`
}

func (s *GatewayServer) handleAdminPutWorkerConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	workerID := mux.Vars(r)["id"]
	var req adminPutWorkerConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	rec, err := s.configStore.UpdateWorkerConfig(workerID, req.WorkerConfigYAML, req.AgentConfigYAML)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		case errors.Is(err, errInvalid):
			http.Error(w, "Bad Request", http.StatusBadRequest)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"worker_id":              rec.WorkerID,
		"config_version":         rec.ConfigVersion,
		"applied_config_version": rec.AppliedConfigVersion,
	})
}

func (s *GatewayServer) handleAdminRotateWorkerToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	workerID := mux.Vars(r)["id"]
	newToken, rec, err := s.configStore.RotateWorkerToken(workerID)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		case errors.Is(err, errInvalid):
			http.Error(w, "Bad Request", http.StatusBadRequest)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"worker_id":      rec.WorkerID,
		"worker_token":   newToken,
		"config_version": rec.ConfigVersion,
	})
}
