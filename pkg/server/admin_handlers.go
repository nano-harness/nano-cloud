package server //nolint:revive

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
	// 1. Check Bearer Token
	if parseBearerToken(r) == s.token {
		return true
	}
	// 2. Check Console Session
	if s.consoleSessionValid(r) {
		return true
	}

	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	return false
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

	s.mu.RLock()
	connectedWorkers := make(map[string]bool)
	for id, sess := range s.workers {
		if sess != nil && sess.Hello != nil {
			connectedWorkers[id] = true
		}
	}
	s.mu.RUnlock()

	out := make([]map[string]any, 0, len(list))
	for _, rec := range list {
		out = append(out, map[string]any{
			"worker_id":       rec.WorkerID,
			"labels":          rec.Labels,
			"created_at_unix": rec.CreatedAtUnix,
			"updated_at_unix": rec.UpdatedAtUnix,
			"online":          connectedWorkers[rec.WorkerID],
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *GatewayServer) handleAdminDeleteWorker(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	workerID := mux.Vars(r)["id"]
	if err := s.configStore.DeleteWorker(workerID); err != nil {
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
	_ = json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
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
		"worker_id":    rec.WorkerID,
		"worker_token": newToken,
	})
}
