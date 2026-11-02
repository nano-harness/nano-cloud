package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
)

type pairingStartRequest struct {
	WorkerName string   `json:"worker_name"`
	HostInfo   string   `json:"host_info"`
	Labels     []string `json:"labels"`
}

type pairingStartResponse struct {
	ID        string `json:"id"`
	UserCode  string `json:"user_code"`
	Secret    string `json:"secret"`
	ExpiresAt int64  `json:"expires_at_unix"`
}

type pairingStatusResponse struct {
	Status      string `json:"status"`
	WorkerToken string `json:"worker_token,omitempty"`
}

func (s *GatewayServer) handleWorkerPairingStart(w http.ResponseWriter, r *http.Request) {
	var req pairingStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	id, secret, userCode, err := s.configStore.CreatePairingRequest(req.WorkerName, req.HostInfo, req.Labels)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pairingStartResponse{
		ID:        id,
		UserCode:  userCode,
		Secret:    secret,
		ExpiresAt: time.Now().Add(PairingTTL).Unix(),
	})
}

func (s *GatewayServer) handleWorkerPairingStatus(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	secret := parseBearerToken(r)
	if secret == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	status, token, err := s.configStore.PollPairingRequest(id, secret)
	if err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		if errors.Is(err, errUnauthorized) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		s.logger.WithError(err).Error("failed to poll pairing request")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pairingStatusResponse{
		Status:      status,
		WorkerToken: token,
	})
}

func (s *GatewayServer) handleAdminListPairingRequests(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	reqs, err := s.configStore.ListPairingRequests()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"requests": reqs})
}

func (s *GatewayServer) handleAdminApprovePairingRequest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := mux.Vars(r)["id"]
	if err := s.configStore.ApprovePairingRequest(id); err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"approved": true})
}

func (s *GatewayServer) handleAdminApprovePairingRequestByCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	code := mux.Vars(r)["code"]
	if err := s.configStore.ApprovePairingRequestByCode(code); err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"approved": true})
}

func (s *GatewayServer) handleAdminRejectPairingRequest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := mux.Vars(r)["id"]
	if err := s.configStore.RejectPairingRequest(id); err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"rejected": true})
}
