package server

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
}

// GatewayServer manages connections and routing
type GatewayServer struct {
	addr             string
	router           *mux.Router
	workers          map[string]*WorkerSession
	workerQueue      map[string][]*runtimev1.Envelope
	streamEvents     map[string][]*runtimev1.Envelope
	streamEventTimes map[string][]time.Time
	streamLastAt     map[string]time.Time
	streamDoneAt     map[string]time.Time
	streamSubs       map[string]map[chan *runtimev1.Envelope]struct{}
	runs             map[string]string
	seqCounter       uint64
	mu               sync.RWMutex
	logger           *logrus.Logger
	token            string
	configStore      *WorkerConfigStore
	consoleSessions  map[string]time.Time
}

const (
	maxWorkerQueuePerWorker  = 1000
	maxStreamEventsPerStream = 2000
	streamRetention          = 10 * time.Minute

	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second
	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second
	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10
)

type WorkerSession struct { //nolint:revive
	ID            string
	Conn          *websocket.Conn
	Hello         *runtimev1.WorkerHello
	LastHeartbeat time.Time
	writeMu       sync.Mutex
}

func NewGatewayServer(addr, token, configStoreDir string) *GatewayServer { //nolint:revive
	return NewGatewayServerWithLogger(addr, token, configStoreDir, logrus.New())
}

func NewGatewayServerWithLogger(addr, token, configStoreDir string, logger *logrus.Logger) *GatewayServer { //nolint:revive
	if logger == nil {
		logger = logrus.New()
	}
	store, err := NewWorkerConfigStore(configStoreDir)
	if err != nil {
		logger.WithError(err).Fatal("failed to initialize worker config store")
	}
	s := &GatewayServer{
		addr:             addr,
		router:           mux.NewRouter(),
		workers:          make(map[string]*WorkerSession),
		workerQueue:      make(map[string][]*runtimev1.Envelope),
		streamEvents:     make(map[string][]*runtimev1.Envelope),
		streamEventTimes: make(map[string][]time.Time),
		streamLastAt:     make(map[string]time.Time),
		streamDoneAt:     make(map[string]time.Time),
		streamSubs:       make(map[string]map[chan *runtimev1.Envelope]struct{}),
		runs:             make(map[string]string),
		logger:           logger,
		token:            token,
		configStore:      store,
		consoleSessions:  make(map[string]time.Time),
	}
	s.setupRoutes()
	return s
}

func (s *GatewayServer) setupRoutes() {
	s.router.HandleFunc("/v1/worker/connect", s.handleWorkerConnect).Methods("GET")
	s.router.HandleFunc("/v1/worker/poll", s.handleWorkerPoll).Methods("POST")
	s.router.HandleFunc("/v1/worker/events", s.handleWorkerEvents).Methods("POST")
	s.router.HandleFunc("/v1/worker/config", s.handleWorkerGetConfig).Methods("GET")
	s.router.HandleFunc("/v1/worker/config/ack", s.handleWorkerConfigAck).Methods("POST")
	s.router.HandleFunc("/v1/worker/pairing", s.handleWorkerPairingStart).Methods("POST")
	s.router.HandleFunc("/v1/worker/pairing/{id}", s.handleWorkerPairingStatus).Methods("GET")
	s.router.HandleFunc("/v1/admin/pairing", s.handleAdminListPairingRequests).Methods("GET")
	s.router.HandleFunc("/v1/admin/pairing/{id}/approve", s.handleAdminApprovePairingRequest).Methods("POST")
	s.router.HandleFunc("/v1/admin/pairing/code/{code}/approve", s.handleAdminApprovePairingRequestByCode).Methods("POST")
	s.router.HandleFunc("/v1/admin/pairing/{id}/reject", s.handleAdminRejectPairingRequest).Methods("POST")
	s.router.HandleFunc("/v1/admin/workers", s.handleAdminListWorkers).Methods("GET")
	s.router.HandleFunc("/v1/admin/workers/{id}/config", s.handleAdminGetWorkerConfig).Methods("GET")
	s.router.HandleFunc("/v1/admin/workers/{id}/config", s.handleAdminPutWorkerConfig).Methods("PUT")
	s.router.HandleFunc("/v1/admin/workers/{id}", s.handleAdminDeleteWorker).Methods("DELETE")
	s.router.HandleFunc("/v1/admin/workers/{id}/rotate-token", s.handleAdminRotateWorkerToken).Methods("POST")
	s.router.HandleFunc("/v1/workers", s.handleListWorkers).Methods("GET")
	s.router.HandleFunc("/v1/runs", s.handleCreateRun).Methods("POST")
	s.router.HandleFunc("/v1/runs/{id}/events", s.handleRunEvents).Methods("GET")
	s.router.HandleFunc("/v1/runs/{id}/cancel", s.handleCancelRun).Methods("POST")
	s.router.HandleFunc("/console", s.handleConsole).Methods("GET")
	s.router.HandleFunc("/console/runs/{id}", s.handleConsoleRunDetail).Methods("GET")
	s.router.HandleFunc("/console/login", s.handleConsoleLogin).Methods("POST")
	s.router.HandleFunc("/console/logout", s.handleConsoleLogout).Methods("POST")
}

func parseConsoleSessionTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("CONSOLE_SESSION_TTL_MINUTES"))
	if v == "" {
		return 8 * time.Hour
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 8 * time.Hour
	}
	return time.Duration(n) * time.Minute
}

func (s *GatewayServer) checkToken(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	if parseBearerToken(r) == s.token {
		return true
	}
	if r.Header.Get("X-Gateway-Token") == s.token {
		return true
	}
	if allowTokenQuery() {
		if strings.TrimSpace(r.URL.Query().Get("token")) == s.token {
			return true
		}
	}
	return false
}

func (s *GatewayServer) Start() error { //nolint:revive
	s.logger.Infof("Starting Gateway Server on %s", s.addr)
	handler := http.Handler(s.router)
	handler = s.recoverMiddleware(handler)
	handler = s.accessLogMiddleware(handler)

	go s.cleanupLoop()

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response does not implement http.Hijacker")
	}
	return h.Hijack()
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (s *GatewayServer) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)

		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host == "" {
			host = r.RemoteAddr
		}

		s.logger.WithFields(logrus.Fields{
			"method":  r.Method,
			"path":    r.URL.Path,
			"status":  sw.status,
			"bytes":   sw.bytes,
			"elapsed": elapsed.String(),
			"remote":  host,
		}).Debug("http")
	})
}

func (s *GatewayServer) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.WithField("panic", rec).Error("panic")
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *GatewayServer) checkBearerToken(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix && auth[len(prefix):] == s.token {
		return true
	}
	return false
}

func (s *GatewayServer) nextSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqCounter++
	return s.seqCounter
}

func (s *GatewayServer) enqueueControl(workerID string, env *runtimev1.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerQueue[workerID] = append(s.workerQueue[workerID], env)
	if len(s.workerQueue[workerID]) > maxWorkerQueuePerWorker {
		s.workerQueue[workerID] = s.workerQueue[workerID][len(s.workerQueue[workerID])-maxWorkerQueuePerWorker:]
	}
}

func (s *GatewayServer) appendStreamEvent(streamID string, env *runtimev1.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamEvents[streamID] = append(s.streamEvents[streamID], env)
	s.streamEventTimes[streamID] = append(s.streamEventTimes[streamID], time.Now())
	s.streamLastAt[streamID] = time.Now()
	if len(s.streamEvents[streamID]) > maxStreamEventsPerStream {
		s.streamEvents[streamID] = s.streamEvents[streamID][len(s.streamEvents[streamID])-maxStreamEventsPerStream:]
		if len(s.streamEventTimes[streamID]) > maxStreamEventsPerStream {
			s.streamEventTimes[streamID] = s.streamEventTimes[streamID][len(s.streamEventTimes[streamID])-maxStreamEventsPerStream:]
		}
	}
	if evt := env.GetRunEvent(); evt != nil && evt.Kind == runtimev1.EventKind_EVENT_KIND_COMPLETED {
		s.streamDoneAt[streamID] = time.Now()
		delete(s.runs, streamID)
	}
	if subs := s.streamSubs[streamID]; len(subs) > 0 {
		for ch := range subs {
			select {
			case ch <- env:
			default:
			}
		}
	}
}

func (s *GatewayServer) DispatchRun(workerID string, streamID string, req *runtimev1.RunRequest) error { //nolint:revive
	env := &runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        workerID,
		StreamId:        streamID,
		Seq:             s.nextSeq(),
		Message:         &runtimev1.Envelope_RunRequest{RunRequest: req},
	}

	s.mu.RLock()
	worker := s.workers[workerID]
	s.mu.RUnlock()

	if worker == nil {
		s.enqueueControl(workerID, env)
		return nil
	}

	if err := s.writeWorkerEnvelope(worker, env); err != nil {
		s.enqueueControl(workerID, env)
		return err
	}
	return nil
}

func (s *GatewayServer) DispatchCancel(workerID string, streamID string, reason string) error { //nolint:revive
	env := &runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        workerID,
		StreamId:        streamID,
		Seq:             s.nextSeq(),
		Message:         &runtimev1.Envelope_CancelRequest{CancelRequest: &runtimev1.CancelRequest{Reason: reason}},
	}

	s.mu.RLock()
	worker := s.workers[workerID]
	s.mu.RUnlock()

	if worker == nil {
		s.enqueueControl(workerID, env)
		return nil
	}

	if err := s.writeWorkerEnvelope(worker, env); err != nil {
		s.enqueueControl(workerID, env)
		return err
	}
	return nil
}

func (s *GatewayServer) writeWorkerEnvelope(worker *WorkerSession, env *runtimev1.Envelope) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	worker.writeMu.Lock()
	defer worker.writeMu.Unlock()
	return worker.Conn.WriteMessage(websocket.BinaryMessage, data)
}

func (s *GatewayServer) readWorkerEnvelope(conn *websocket.Conn) (*runtimev1.Envelope, error) {
	msgType, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if msgType != websocket.BinaryMessage {
		return nil, fmt.Errorf("unexpected websocket message type: %d", msgType)
	}
	var env runtimev1.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

func (s *GatewayServer) handleWorkerConnect(w http.ResponseWriter, r *http.Request) {
	expectedWorkerID := ""
	if !s.checkToken(r) {
		tok := parseBearerToken(r)
		if tok == "" {
			tok = r.Header.Get("X-Worker-Token")
		}
		if tok == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		rec, err := s.configStore.GetConfigByWorkerToken(tok)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		expectedWorkerID = rec.WorkerID
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Errorf("Failed to upgrade worker connection: %v", err)
		return
	}
	defer conn.Close() //nolint:errcheck

	conn.SetReadDeadline(time.Now().Add(pongWait))                                                         //nolint:errcheck
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(pongWait)); return nil }) //nolint:errcheck

	env, err := s.readWorkerEnvelope(conn)
	if err != nil {
		s.logger.Errorf("Failed to read worker hello: %v", err)
		return
	}
	hello := env.GetWorkerHello()
	if hello == nil {
		s.logger.Errorf("Expected worker_hello envelope, got %T", env.Message)
		return
	}

	workerID := env.WorkerId
	if workerID == "" {
		if expectedWorkerID != "" {
			s.logger.Errorf("Worker token is bound to %q but hello missing worker_id", expectedWorkerID)
			return
		}
		workerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if expectedWorkerID != "" && workerID != expectedWorkerID {
		s.logger.Errorf("Worker token is bound to %q but hello worker_id=%q", expectedWorkerID, workerID)
		return
	}

	worker := &WorkerSession{
		ID:            workerID,
		Conn:          conn,
		Hello:         hello,
		LastHeartbeat: time.Now(),
	}

	s.mu.Lock()
	s.workers[workerID] = worker
	s.mu.Unlock()

	s.logger.Infof("Worker connected: %s (%s)", hello.Name, workerID)

	s.flushWorkerQueue(workerID, worker)

	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for range ticker.C {
			worker.writeMu.Lock()
			conn.SetWriteDeadline(time.Now().Add(writeWait)) //nolint:errcheck
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				worker.writeMu.Unlock()
				return
			}
			worker.writeMu.Unlock()
		}
	}()

	for {
		inEnv, err := s.readWorkerEnvelope(conn)
		if err != nil {
			s.logger.Infof("Worker disconnected: %s (%v)", workerID, err)
			break
		}
		conn.SetReadDeadline(time.Now().Add(pongWait)) //nolint:errcheck

		if hb := inEnv.GetWorkerHeartbeat(); hb != nil {
			s.mu.Lock()
			worker.LastHeartbeat = time.Now()
			s.mu.Unlock()
			continue
		}

		if evt := inEnv.GetRunEvent(); evt != nil && inEnv.StreamId != "" {
			s.appendStreamEvent(inEnv.StreamId, inEnv)
			continue
		}
	}

	s.mu.Lock()
	delete(s.workers, workerID)
	s.mu.Unlock()
}

func (s *GatewayServer) flushWorkerQueue(workerID string, worker *WorkerSession) {
	s.mu.Lock()
	queue := s.workerQueue[workerID]
	delete(s.workerQueue, workerID)
	s.mu.Unlock()

	for i := 0; i < len(queue); i++ {
		if err := s.writeWorkerEnvelope(worker, queue[i]); err != nil {
			s.logger.WithError(err).Warnf("failed to flush queued message to worker_id=%s", workerID)
			for j := i; j < len(queue); j++ {
				s.enqueueControl(workerID, queue[j])
			}
			return
		}
	}
}

func (s *GatewayServer) cleanupLoop() {
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()

	for range t.C {
		now := time.Now()
		cutoff := now.Add(-streamRetention)
		s.mu.Lock()
		for streamID, doneAt := range s.streamDoneAt {
			if doneAt.After(cutoff) {
				continue
			}
			if subs := s.streamSubs[streamID]; len(subs) > 0 {
				continue
			}
			delete(s.streamEvents, streamID)
			delete(s.streamEventTimes, streamID)
			delete(s.streamDoneAt, streamID)
			delete(s.streamLastAt, streamID)
			delete(s.streamSubs, streamID)
			delete(s.runs, streamID)
		}
		for streamID, lastAt := range s.streamLastAt {
			if _, done := s.streamDoneAt[streamID]; done {
				continue
			}
			if lastAt.After(cutoff) {
				continue
			}
			if subs := s.streamSubs[streamID]; len(subs) > 0 {
				continue
			}
			delete(s.streamEvents, streamID)
			delete(s.streamEventTimes, streamID)
			delete(s.streamLastAt, streamID)
			delete(s.streamSubs, streamID)
			delete(s.runs, streamID)
		}
		for sid, exp := range s.consoleSessions {
			if !exp.After(now) {
				delete(s.consoleSessions, sid)
			}
		}
		s.mu.Unlock()
	}
}

type workerPollRequest struct {
	WorkerID    string `json:"worker_id"`
	LastSeq     uint64 `json:"last_seq"`
	MaxMessages int    `json:"max_messages"`
}

type workerPollResponse struct {
	Messages []json.RawMessage `json:"messages"`
}

func (s *GatewayServer) handleWorkerPoll(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	var req workerPollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if req.MaxMessages <= 0 {
		req.MaxMessages = 100
	}
	if req.MaxMessages > 1000 {
		req.MaxMessages = 1000
	}

	s.mu.Lock()
	queue := s.workerQueue[req.WorkerID]
	out := make([]*runtimev1.Envelope, 0, req.MaxMessages)
	remaining := make([]*runtimev1.Envelope, 0, len(queue))
	for _, env := range queue {
		if env.Seq <= req.LastSeq {
			continue
		}
		if len(out) < req.MaxMessages {
			out = append(out, env)
			continue
		}
		remaining = append(remaining, env)
	}
	s.workerQueue[req.WorkerID] = remaining
	s.mu.Unlock()

	marshaler := protojson.MarshalOptions{UseProtoNames: true}
	resp := workerPollResponse{Messages: make([]json.RawMessage, 0, len(out))}
	for _, env := range out {
		b, err := marshaler.Marshal(env)
		if err != nil {
			continue
		}
		resp.Messages = append(resp.Messages, json.RawMessage(b))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

type workerEventsRequest struct {
	WorkerID  string            `json:"worker_id"`
	Messages  []json.RawMessage `json:"messages"`
	TunnelID  string            `json:"tunnel_id,omitempty"`
	WorkerSeq uint64            `json:"seq,omitempty"`
}

type workerEventsAck struct {
	Accepted bool   `json:"accepted"`
	Detail   string `json:"detail,omitempty"`
}

func (s *GatewayServer) handleWorkerEvents(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req workerEventsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	accepted := 0
	for _, raw := range req.Messages {
		var env runtimev1.Envelope
		if err := unmarshaler.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.StreamId == "" {
			continue
		}
		if env.GetRunEvent() == nil {
			continue
		}
		s.appendStreamEvent(env.StreamId, &env)
		accepted++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(workerEventsAck{Accepted: true, Detail: fmt.Sprintf("accepted=%d", accepted)}) //nolint:errcheck
}

type workerInfo struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Labels            []string `json:"labels"`
	SupportedRuntimes []string `json:"supported_runtimes"`
	LastSeen          int64    `json:"last_seen_unix_millis"`
}

func (s *GatewayServer) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]workerInfo, 0, len(s.workers))
	for id, sess := range s.workers {
		if sess == nil || sess.Hello == nil {
			continue
		}
		rts := make([]string, 0, len(sess.Hello.SupportedRuntimes))
		for _, rt := range sess.Hello.SupportedRuntimes {
			rts = append(rts, strings.ToLower(strings.TrimPrefix(rt.String(), "RUNTIME_")))
		}
		list = append(list, workerInfo{
			ID:                id,
			Name:              sess.Hello.Name,
			Version:           sess.Hello.Version,
			Labels:            sess.Hello.Labels,
			SupportedRuntimes: rts,
			LastSeen:          sess.LastHeartbeat.UnixMilli(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list) //nolint:errcheck
}

type createRunRequest struct {
	Runtime        string              `json:"runtime"`
	Prompt         string              `json:"prompt"`
	SessionID      string              `json:"session_id,omitempty"`
	WorkerID       string              `json:"worker_id,omitempty"`
	WorkspaceID    string              `json:"workspace_id,omitempty"`
	TimeoutSeconds uint32              `json:"timeout_seconds,omitempty"`
	Resources      *runtimev1.Capacity `json:"resources,omitempty"`
	Network        string              `json:"network,omitempty"`
	Approval       string              `json:"approval,omitempty"`
}

type createRunResponse struct {
	RunID     string `json:"run_id"`
	WorkerID  string `json:"worker_id"`
	EventsURL string `json:"events_url"`
}

func (s *GatewayServer) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	runtime := parseRuntime(req.Runtime)
	if runtime == runtimev1.Runtime_RUNTIME_UNSPECIFIED {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	workerID := strings.TrimSpace(req.WorkerID)
	if workerID == "" {
		workerID = s.pickWorkerForRuntime(runtime)
	}
	if workerID == "" {
		http.Error(w, "No worker available", http.StatusServiceUnavailable)
		return
	}

	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	timeout := req.TimeoutSeconds
	if timeout == 0 {
		timeout = 300
	}

	network := runtimev1.NetworkPolicy_NETWORK_POLICY_ALL
	if strings.TrimSpace(req.Network) != "" {
		v, ok := parseNetworkPolicy(req.Network)
		if !ok {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		network = v
	}

	approval := runtimev1.ApprovalPolicy_APPROVAL_POLICY_AUTO
	if strings.TrimSpace(req.Approval) != "" {
		v, ok := parseApprovalPolicy(req.Approval)
		if !ok {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		approval = v
	}

	resources := req.Resources
	if resources == nil {
		resources = &runtimev1.Capacity{}
	}

	runReq := &runtimev1.RunRequest{
		SessionId: req.SessionID,
		Runtime:   runtime,
		Workspace: &runtimev1.WorkspaceSpec{
			WorkspaceId: req.WorkspaceID,
			MountMode:   runtimev1.MountMode_MOUNT_MODE_VOLUME,
			Workdir:     "/workspace",
			Persistent:  false,
			Source:      &runtimev1.WorkspaceSource{Source: &runtimev1.WorkspaceSource_Empty{Empty: &runtimev1.EmptySource{}}},
		},
		Input: &runtimev1.Input{
			Messages: []*runtimev1.Message{
				{Role: runtimev1.Role_ROLE_USER, Content: req.Prompt},
			},
		},
		Policy: &runtimev1.Policy{
			TimeoutSeconds: timeout,
			Resources:      resources,
			Network:        network,
			Approval:       approval,
		},
	}

	_ = s.DispatchRun(workerID, runID, runReq)

	s.mu.Lock()
	s.runs[runID] = workerID
	s.streamLastAt[runID] = time.Now()
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(createRunResponse{ //nolint:errcheck
		RunID:     runID,
		WorkerID:  workerID,
		EventsURL: fmt.Sprintf("/v1/runs/%s/events", runID),
	})
}

func allowTokenQuery() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ALLOW_TOKEN_QUERY")))
	return v == "1" || v == "true" || v == "yes" || v == "on" || v == "dev" || strings.ToLower(strings.TrimSpace(os.Getenv("DEV_MODE"))) == "true"
}

func parseNetworkPolicy(s string) (runtimev1.NetworkPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "deny", "off":
		return runtimev1.NetworkPolicy_NETWORK_POLICY_NONE, true
	case "allowlist", "allow-list":
		return runtimev1.NetworkPolicy_NETWORK_POLICY_ALLOWLIST, true
	case "all", "allow":
		return runtimev1.NetworkPolicy_NETWORK_POLICY_ALL, true
	default:
		return runtimev1.NetworkPolicy_NETWORK_POLICY_UNSPECIFIED, false
	}
}

func parseApprovalPolicy(s string) (runtimev1.ApprovalPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return runtimev1.ApprovalPolicy_APPROVAL_POLICY_AUTO, true
	case "ask":
		return runtimev1.ApprovalPolicy_APPROVAL_POLICY_ASK, true
	case "deny", "none":
		return runtimev1.ApprovalPolicy_APPROVAL_POLICY_DENY, true
	default:
		return runtimev1.ApprovalPolicy_APPROVAL_POLICY_UNSPECIFIED, false
	}
}

func parseRuntime(s string) runtimev1.Runtime {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "nano_agent", "nano-agent", "nano":
		return runtimev1.Runtime_RUNTIME_NANO_AGENT
	case "claude_code", "claude-code", "claude":
		return runtimev1.Runtime_RUNTIME_CLAUDE_CODE
	case "opencode", "open-code":
		return runtimev1.Runtime_RUNTIME_OPENCODE
	case "custom":
		return runtimev1.Runtime_RUNTIME_CUSTOM
	default:
		return runtimev1.Runtime_RUNTIME_UNSPECIFIED
	}
}

func (s *GatewayServer) pickWorkerForRuntime(rt runtimev1.Runtime) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, w := range s.workers {
		if w == nil || w.Hello == nil {
			continue
		}
		for _, supported := range w.Hello.SupportedRuntimes {
			if supported == rt || supported == runtimev1.Runtime_RUNTIME_CUSTOM {
				return id
			}
		}
	}
	return ""
}

func (s *GatewayServer) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	s.mu.RLock()
	workerID := s.runs[id]
	s.mu.RUnlock()
	if workerID == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	_ = s.DispatchCancel(workerID, id, "cancel requested by client")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"accepted": true}) //nolint:errcheck
}

func (s *GatewayServer) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan *runtimev1.Envelope, 64)

	s.mu.Lock()
	if s.streamSubs[id] == nil {
		s.streamSubs[id] = make(map[chan *runtimev1.Envelope]struct{})
	}
	s.streamSubs[id][ch] = struct{}{}
	history := append([]*runtimev1.Envelope(nil), s.streamEvents[id]...)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if subs := s.streamSubs[id]; subs != nil {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(s.streamSubs, id)
			}
		}
		s.mu.Unlock()
		close(ch)
	}()

	marshaler := protojson.MarshalOptions{UseProtoNames: true}
	writeEvent := func(env *runtimev1.Envelope) {
		b, err := marshaler.Marshal(env)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: run_event\n")      //nolint:errcheck
		fmt.Fprintf(w, "data: %s\n\n", string(b)) //nolint:errcheck
		flusher.Flush()
	}

	for _, env := range history {
		writeEvent(env)
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case env := <-ch:
			if env != nil {
				writeEvent(env)
			}
		case <-keepalive.C:
			fmt.Fprintf(w, "event: ping\ndata: {}\n\n") //nolint:errcheck
			flusher.Flush()
		}
	}
}

func (s *GatewayServer) handleConsole(w http.ResponseWriter, r *http.Request) {
	authEnabled := s.consoleAuthEnabled()
	loggedIn := s.consoleSessionValid(r)
	loginFailed := r.URL.Query().Get("login") == "failed"

	type consoleWorkerRow struct {
		IDPrefix        string
		Name            string
		Version         string
		Labels          string
		Supported       string
		LastSeenUnixSec int64
		ConfigPrefix    string
		AppliedPrefix   string
		ConfigApplied   bool
	}
	type consoleRunRow struct {
		RunIDPrefix    string
		WorkerIDPrefix string
		EventCount     int
		LastSeq        uint64
	}
	type consolePublicWorkerRow struct {
		Name            string
		Version         string
		Supported       string
		LastSeenUnixSec int64
	}

	now := time.Now()

	configByWorkerID := map[string]WorkerRecord{}
	if s.configStore != nil {
		if list, err := s.configStore.ListWorkers(); err == nil {
			for _, rec := range list {
				configByWorkerID[rec.WorkerID] = rec
			}
		}
	}

	s.mu.RLock()
	publicWorkers := make([]consolePublicWorkerRow, 0, len(s.workers))
	workers := make([]consoleWorkerRow, 0, len(s.workers))
	for id, sess := range s.workers {
		if sess == nil || sess.Hello == nil {
			continue
		}
		idPrefix := id
		if len(idPrefix) > 12 {
			idPrefix = idPrefix[:12]
		}
		labels := strings.Join(sess.Hello.Labels, ",")
		rts := make([]string, 0, len(sess.Hello.SupportedRuntimes))
		for _, rt := range sess.Hello.SupportedRuntimes {
			rts = append(rts, strings.ToLower(strings.TrimPrefix(rt.String(), "RUNTIME_")))
		}
		publicWorkers = append(publicWorkers, consolePublicWorkerRow{
			Name:            sess.Hello.Name,
			Version:         sess.Hello.Version,
			Supported:       strings.Join(rts, ","),
			LastSeenUnixSec: sess.LastHeartbeat.Unix(),
		})
		rec, ok := configByWorkerID[id]
		configPrefix := ""
		appliedPrefix := ""
		appliedOK := false
		if ok {
			configPrefix = rec.ConfigVersion
			if len(configPrefix) > 12 {
				configPrefix = configPrefix[:12]
			}
			appliedPrefix = rec.AppliedConfigVersion
			if len(appliedPrefix) > 12 {
				appliedPrefix = appliedPrefix[:12]
			}
			appliedOK = rec.ConfigVersion != "" && rec.ConfigVersion == rec.AppliedConfigVersion
		}
		workers = append(workers, consoleWorkerRow{
			IDPrefix:        idPrefix,
			Name:            sess.Hello.Name,
			Version:         sess.Hello.Version,
			Labels:          labels,
			Supported:       strings.Join(rts, ","),
			LastSeenUnixSec: sess.LastHeartbeat.Unix(),
			ConfigPrefix:    configPrefix,
			AppliedPrefix:   appliedPrefix,
			ConfigApplied:   appliedOK,
		})
	}

	runs := make([]consoleRunRow, 0, len(s.runs))
	for runID, workerID := range s.runs {
		rid := runID
		if len(rid) > 12 {
			rid = rid[:12]
		}
		wid := workerID
		if len(wid) > 12 {
			wid = wid[:12]
		}
		events := s.streamEvents[runID]
		var lastSeq uint64
		if len(events) > 0 {
			lastSeq = events[len(events)-1].Seq
		}
		runs = append(runs, consoleRunRow{
			RunIDPrefix:    rid,
			WorkerIDPrefix: wid,
			EventCount:     len(events),
			LastSeq:        lastSeq,
		})
	}
	s.mu.RUnlock()

	sort.Slice(publicWorkers, func(i, j int) bool { return publicWorkers[i].LastSeenUnixSec > publicWorkers[j].LastSeenUnixSec })
	sort.Slice(workers, func(i, j int) bool { return workers[i].LastSeenUnixSec > workers[j].LastSeenUnixSec })
	sort.Slice(runs, func(i, j int) bool { return runs[i].LastSeq > runs[j].LastSeq })
	if len(runs) > 30 {
		runs = runs[:30]
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")

	fmt.Fprint(w, "<!DOCTYPE html><html><head><meta charset=\"utf-8\" /><title>Nano Cloud Console</title>")                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             //nolint:errcheck
	fmt.Fprint(w, "<style>body{font-family:system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;padding:24px;background:#0d1117;color:#e6edf3;max-width:1200px;margin:0 auto;line-height:1.5}h1{margin:0 0 12px 0;font-size:24px;font-weight:600}h2{margin:24px 0 12px 0;font-size:14px;letter-spacing:1px;color:#8b949e;text-transform:uppercase}table{border-collapse:collapse;width:100%;background:#161b22;border-radius:6px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,0.1)}th,td{border-bottom:1px solid #30363d;padding:12px 16px;font-size:13px}th{background:#21262d;text-align:left;font-weight:600;color:#8b949e}tr{transition:background 0.2s ease}tr:hover{background:#1f242c}tr:last-child td{border-bottom:none}code{font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;background:rgba(110,118,129,0.4);padding:2px 6px;border-radius:4px;font-size:12px}small{color:#8b949e}.ok{color:#3fb950;display:inline-flex;align-items:center;gap:4px}.ok::before{content:'';display:inline-block;width:8px;height:8px;background:#3fb950;border-radius:50%}.bad{color:#f85149;display:inline-flex;align-items:center;gap:4px}.bad::before{content:'';display:inline-block;width:8px;height:8px;background:#f85149;border-radius:50%}.panel{margin-top:16px;border:1px solid #30363d;padding:20px;border-radius:8px;background:#161b22;box-shadow:0 1px 3px rgba(0,0,0,0.1)}label{display:block;margin-top:12px;font-size:14px;font-weight:500}.field{margin-top:6px;padding:8px 12px;border-radius:6px;border:1px solid #30363d;background:#0d1117;color:#e6edf3;width:100%;box-sizing:border-box;transition:all 0.2s ease}.field:focus{outline:none;border-color:#58a6ff;box-shadow:0 0 0 3px rgba(88,166,255,0.3)}.btn{margin-top:12px;padding:8px 16px;border-radius:6px;border:1px solid rgba(240,246,252,0.1);background:#238636;color:white;cursor:pointer;font-weight:500;transition:all 0.2s ease;display:inline-flex;align-items:center;justify-content:center}.btn:hover{background:#2ea043;border-color:rgba(240,246,252,0.1)}.btn:active{background:#238636;transform:scale(0.98)}.btn:focus-visible{outline:none;box-shadow:0 0 0 3px rgba(46,160,67,0.4)}.btn.secondary{background:#21262d;border-color:rgba(240,246,252,0.1);color:#c9d1d9}.btn.secondary:hover{background:#30363d;border-color:#8b949e}.btn.danger{background:#da3633;color:white;border-color:rgba(240,246,252,0.1)}.btn.danger:hover{background:#f85149}.err{color:#f85149;margin-top:8px;padding:8px 12px;background:rgba(248,81,73,0.1);border-left:4px solid #f85149;border-radius:4px}.empty-state{padding:32px;text-align:center;color:#8b949e;background:#161b22;border:1px dashed #30363d;border-radius:6px;font-size:14px}.action-form{display:inline-flex;gap:8px;align-items:center}a{color:#58a6ff;text-decoration:none}a:hover{text-decoration:underline}</style>") //nolint:errcheck
	fmt.Fprint(w, "</head><body>")                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      //nolint:errcheck
	fmt.Fprint(w, "<h1>Nano Cloud Console</h1>")                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        //nolint:errcheck
	fmt.Fprintf(w, "<small>server_time=%s | workers_online=%d | runs_tracked=%d</small>", now.Format(time.RFC3339), len(publicWorkers), len(runs))                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      //nolint:errcheck

	if authEnabled && !loggedIn {
		fmt.Fprint(w, "<div class=\"panel\"><h2>LOGIN REQUIRED FOR SENSITIVE DATA</h2>") //nolint:errcheck
		fmt.Fprint(w, "<small>匿名访问仅展示公开概览，敏感字段需要登录。</small>")                            //nolint:errcheck
		if loginFailed {
			fmt.Fprint(w, "<div class=\"err\">Token 错误</div>") //nolint:errcheck
		}
		fmt.Fprint(w, "<form method=\"post\" action=\"/console/login\">")                                                                         //nolint:errcheck
		fmt.Fprint(w, "<label>Gateway Token</label><input class=\"field\" type=\"password\" name=\"token\" autocomplete=\"current-password\" />") //nolint:errcheck
		fmt.Fprint(w, "<button class=\"btn\" type=\"submit\">登录</button></form></div>")                                                           //nolint:errcheck
	}
	if authEnabled && loggedIn {
		fmt.Fprint(w, "<div class=\"panel\"><small>已登录，可查看敏感信息</small>")                                                                             //nolint:errcheck
		fmt.Fprint(w, "<form method=\"post\" action=\"/console/logout\"><button class=\"btn secondary\" type=\"submit\">退出登录</button></form></div>") //nolint:errcheck
	}
	if !authEnabled {
		fmt.Fprint(w, "<div class=\"panel\"><small>未配置 -token，敏感信息默认隐藏。</small></div>") //nolint:errcheck
	}

	// Pairing Requests Section (Only visible when logged in)
	if loggedIn {
		type pairingRow struct {
			ID        string
			UserCode  string
			Worker    string
			Host      string
			Labels    string
			Status    string
			CreatedAt string
		}
		var pending []pairingRow
		if reqs, err := s.configStore.ListPairingRequests(); err == nil {
			for _, r := range reqs {
				if r.Status == "pending" {
					pending = append(pending, pairingRow{
						ID:        r.ID,
						UserCode:  r.UserCode,
						Worker:    r.WorkerName,
						Host:      r.HostInfo,
						Labels:    strings.Join(r.Labels, ","),
						Status:    r.Status,
						CreatedAt: time.Unix(r.CreatedAt, 0).Format(time.RFC3339),
					})
				}
			}
		}

		if len(pending) > 0 {
			fmt.Fprint(w, "<h2>PENDING PAIRING REQUESTS</h2>")                                                                                             //nolint:errcheck
			fmt.Fprint(w, "<table><thead><tr><th>code</th><th>worker</th><th>host</th><th>labels</th><th>created</th><th>action</th></tr></thead><tbody>") //nolint:errcheck
			for _, row := range pending {
				actionForm := fmt.Sprintf(`<div class="action-form"><form method="post" action="/v1/admin/pairing/%s/approve"><button class="btn" style="margin-top:0;padding:6px 12px;">Approve</button></form><form method="post" action="/v1/admin/pairing/%s/reject"><button class="btn danger" style="margin-top:0;padding:6px 12px;">Reject</button></form></div>`, row.ID, row.ID)
				fmt.Fprintf(w, "<tr><td><strong>%s</strong></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>", htmlEscape(row.UserCode), htmlEscape(row.Worker), htmlEscape(row.Host), htmlEscape(row.Labels), row.CreatedAt, actionForm) //nolint:errcheck
			}
			fmt.Fprint(w, "</tbody></table>") //nolint:errcheck
		} else {
			fmt.Fprint(w, "<h2>PENDING PAIRING REQUESTS</h2>")                             //nolint:errcheck
			fmt.Fprint(w, "<div class=\"empty-state\">No pending pairing requests.</div>") //nolint:errcheck
		}

		// Quick Approve by Code form
		prefillCode := r.URL.Query().Get("pairing")
		_, _ = fmt.Fprintf(w, `<div class="panel">
			<h3 style="margin-top:0">Approve by Code</h3>
			<form method="post" action="/v1/admin/pairing/code/APPROVE_CODE/approve" onsubmit="this.action='/v1/admin/pairing/code/' + document.getElementById('short_code').value + '/approve'; return true;" style="display:flex; gap:12px; align-items:center; margin-top:12px;">
				<input class="field" id="short_code" type="text" placeholder="Enter 6-character code" required pattern="[a-zA-Z0-9]{6}" style="margin:0; width:240px;" value="%s" />
				<button class="btn" type="submit" style="margin:0;">Approve Worker</button>
			</form>
		</div>`, htmlEscape(prefillCode))
	}

	fmt.Fprint(w, "<h2>PUBLIC WORKERS</h2>") //nolint:errcheck
	if len(publicWorkers) == 0 {
		fmt.Fprint(w, "<div class=\"empty-state\">No public workers connected.</div>") //nolint:errcheck
	} else {
		fmt.Fprint(w, "<table><thead><tr><th>name</th><th>version</th><th>runtimes</th><th>last_seen</th></tr></thead><tbody>") //nolint:errcheck
		for _, row := range publicWorkers {
			last := time.Unix(row.LastSeenUnixSec, 0).Format(time.RFC3339)
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td><code>%s</code></td></tr>", htmlEscape(row.Name), htmlEscape(row.Version), htmlEscape(row.Supported), last) //nolint:errcheck
		}
		fmt.Fprint(w, "</tbody></table>") //nolint:errcheck
	}

	if !loggedIn {
		fmt.Fprint(w, "</body></html>") //nolint:errcheck
		return
	}

	fmt.Fprint(w, "<h2>PRIVATE WORKERS</h2>") //nolint:errcheck
	if len(workers) == 0 {
		fmt.Fprint(w, "<div class=\"empty-state\">No private workers registered.</div>") //nolint:errcheck
	} else {
		fmt.Fprint(w, "<table><thead><tr><th>id</th><th>name</th><th>version</th><th>labels</th><th>runtimes</th><th>config</th><th>applied</th><th>status</th><th>last_seen</th></tr></thead><tbody>") //nolint:errcheck
		for _, row := range workers {
			last := time.Unix(row.LastSeenUnixSec, 0).Format(time.RFC3339)
			statusClass := "bad"
			statusText := "pending"
			if row.ConfigApplied {
				statusClass = "ok"
				statusText = "applied"
			}
			fmt.Fprintf(w, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td><code>%s</code></td><td><code>%s</code></td><td><span class=\"%s\">%s</span></td><td><code>%s</code></td></tr>", htmlEscape(row.IDPrefix), htmlEscape(row.Name), htmlEscape(row.Version), htmlEscape(row.Labels), htmlEscape(row.Supported), htmlEscape(row.ConfigPrefix), htmlEscape(row.AppliedPrefix), statusClass, statusText, last) //nolint:errcheck
		}
		fmt.Fprint(w, "</tbody></table>") //nolint:errcheck
	}

	baseURL := "http://" + r.Host
	if r.TLS != nil {
		baseURL = "https://" + r.Host
	}
	fmt.Fprint(w, "<h2>RECENT RUNS</h2>") //nolint:errcheck
	if len(runs) == 0 {
		fmt.Fprint(w, "<div class=\"empty-state\">No runs have been executed yet.</div>") //nolint:errcheck
	} else {
		fmt.Fprint(w, "<table><thead><tr><th>run</th><th>worker</th><th>events</th><th>last_seq</th><th>action</th></tr></thead><tbody>") //nolint:errcheck
		for _, row := range runs {
			eventsURL := fmt.Sprintf("%s/v1/runs/%s/events", baseURL, row.RunIDPrefix)
			cmdCurl := fmt.Sprintf(`curl -NsS "%s" -H "Authorization: Bearer <token>"`, eventsURL)
			cmdLogs := fmt.Sprintf("worker logs %s --follow", row.RunIDPrefix)
			detailURL := fmt.Sprintf("/console/runs/%s", row.RunIDPrefix)
			actionHTML := fmt.Sprintf(`<a href="%s" target="_blank">events</a> · <a href="%s">detail</a><br/><code style="display:block;margin-top:4px">%s</code><code style="display:block;margin-top:4px">%s</code>`, eventsURL, detailURL, htmlEscape(cmdCurl), htmlEscape(cmdLogs))
			fmt.Fprintf(w, "<tr><td><code>%s</code></td><td><code>%s</code></td><td>%d</td><td><code>%d</code></td><td>%s</td></tr>", htmlEscape(row.RunIDPrefix), htmlEscape(row.WorkerIDPrefix), row.EventCount, row.LastSeq, actionHTML) //nolint:errcheck
		}
		fmt.Fprint(w, "</tbody></table>") //nolint:errcheck
	}

	_, _ = fmt.Fprint(w, `<script>
document.querySelectorAll('form').forEach(form => {
	form.addEventListener('submit', (e) => {
		const btn = form.querySelector('button[type="submit"]');
		if (btn) {
			btn.style.opacity = '0.7';
			btn.style.pointerEvents = 'none';
			if (!btn.innerHTML.includes('⏳')) {
				btn.innerHTML = '<span style="display:inline-block;animation:spin 1s linear infinite;margin-right:6px">⏳</span>' + btn.innerHTML;
			}
		}
	});
});
if (!document.querySelector('#spin-style')) {
	const style = document.createElement('style');
	style.id = 'spin-style';
	style.innerHTML = '@keyframes spin { 100% { transform: rotate(360deg); } }';
	document.head.appendChild(style);
}
</script>`)
	fmt.Fprint(w, "</body></html>") //nolint:errcheck
}

func (s *GatewayServer) consoleAuthEnabled() bool {
	return s.token != ""
}

func (s *GatewayServer) consoleSessionValid(r *http.Request) bool {
	if !s.consoleAuthEnabled() {
		return false
	}
	c, err := r.Cookie("nano_console_session")
	if err != nil || strings.TrimSpace(c.Value) == "" {
		return false
	}
	now := time.Now()
	s.mu.RLock()
	exp, ok := s.consoleSessions[c.Value]
	s.mu.RUnlock()
	if !ok || !exp.After(now) {
		if ok {
			s.mu.Lock()
			delete(s.consoleSessions, c.Value)
			s.mu.Unlock()
		}
		return false
	}
	return true
}

func (s *GatewayServer) createConsoleSession() (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	sid := hex.EncodeToString(raw)
	exp := time.Now().Add(parseConsoleSessionTTL())
	s.mu.Lock()
	s.consoleSessions[sid] = exp
	s.mu.Unlock()
	return sid, exp, nil
}

func (s *GatewayServer) clearConsoleSession(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	s.mu.Lock()
	delete(s.consoleSessions, sessionID)
	s.mu.Unlock()
}

func (s *GatewayServer) setConsoleSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     "nano_console_session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	})
}

func (s *GatewayServer) clearConsoleSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "nano_console_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func (s *GatewayServer) handleConsoleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.consoleAuthEnabled() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/console?login=failed", http.StatusSeeOther)
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	// If the form field 'token' is missing, fallback to 'password' for backward compatibility
	if token == "" {
		token = strings.TrimSpace(r.FormValue("password"))
	}

	// Compare with gateway token
	ok := subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
	if !ok {
		http.Redirect(w, r, "/console?login=failed", http.StatusSeeOther)
		return
	}
	sid, exp, err := s.createConsoleSession()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.setConsoleSessionCookie(w, r, sid, exp)
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func (s *GatewayServer) handleConsoleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("nano_console_session"); err == nil {
		s.clearConsoleSession(c.Value)
	}
	s.clearConsoleSessionCookie(w)
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func (s *GatewayServer) handleConsoleRunDetail(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	authEnabled := s.consoleAuthEnabled()
	loggedIn := s.consoleSessionValid(r)
	if authEnabled && !loggedIn {
		http.Redirect(w, r, "/console?login=failed", http.StatusSeeOther)
		return
	}
	s.mu.RLock()
	history := append([]*runtimev1.Envelope(nil), s.streamEvents[id]...)
	times := append([]time.Time(nil), s.streamEventTimes[id]...)
	s.mu.RUnlock()
	stageRows := make([][3]string, 0, 8)
	var lastT time.Time
	for i := range history {
		if i >= len(times) {
			break
		}
		env := history[i]
		if env == nil || env.GetRunEvent() == nil {
			continue
		}
		t := times[i]
		if st := env.GetRunEvent().GetStatus(); st != nil {
			delta := ""
			if !lastT.IsZero() {
				delta = fmt.Sprintf("%d", t.Sub(lastT).Milliseconds())
			}
			stageRows = append(stageRows, [3]string{st.Status, t.Format(time.RFC3339), delta})
			lastT = t
		}
	}
	var lastErrCode, lastErrMsg, hint string
	for i := len(history) - 1; i >= 0; i-- {
		evt := history[i].GetRunEvent()
		if evt == nil {
			continue
		}
		if e := evt.GetError(); e != nil {
			lastErrCode = e.Code
			lastErrMsg = e.Message
			hint = errorHintForCode(e.Code)
			break
		}
	}
	var stats map[string]any
	for i := len(history) - 1; i >= 0; i-- {
		evt := history[i].GetRunEvent()
		if evt == nil {
			continue
		}
		if c := evt.GetCompleted(); c != nil && strings.TrimSpace(c.StatsJson) != "" {
			_ = json.Unmarshal([]byte(c.StatsJson), &stats)
			break
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!DOCTYPE html><html><head><meta charset=\"utf-8\" /><title>Run Detail</title><style>body{font-family:system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;padding:24px;background:#0d1117;color:#e6edf3;max-width:1000px;margin:0 auto;line-height:1.5}h2{margin:0 0 24px 0;font-size:24px;font-weight:600}h3{margin:0 0 16px 0;font-size:16px;color:#c9d1d9}table{border-collapse:collapse;width:100%;background:#161b22;border-radius:6px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,0.1)}th,td{border-bottom:1px solid #30363d;padding:12px 16px;font-size:13px}th{background:#21262d;text-align:left;font-weight:600;color:#8b949e}tr{transition:background 0.2s ease}tr:hover{background:#1f242c}tr:last-child td{border-bottom:none}code{font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;background:rgba(110,118,129,0.4);padding:2px 6px;border-radius:4px;font-size:12px}.panel{margin-top:24px;border:1px solid #30363d;padding:20px;border-radius:8px;background:#161b22;box-shadow:0 1px 3px rgba(0,0,0,0.1)}.err{color:#f85149;padding:12px;background:rgba(248,81,73,0.1);border-left:4px solid #f85149;border-radius:4px;margin-bottom:12px}.ok{color:#3fb950}pre{background:#0d1117;padding:16px;border-radius:6px;border:1px solid #30363d;overflow-x:auto;font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;font-size:13px;color:#e6edf3}.hint{color:#8b949e;font-size:13px;display:flex;align-items:center;gap:6px;margin-top:8px}.hint::before{content:'💡';font-size:14px}a.back{display:inline-flex;align-items:center;gap:6px;color:#8b949e;text-decoration:none;margin-bottom:24px;transition:color 0.2s ease}a.back:hover{color:#e6edf3}a.back::before{content:'←'}</style></head><body>") //nolint:errcheck
	fmt.Fprintf(w, "<a href=\"/console\" class=\"back\">Back to Console</a><h2>Run %s</h2>", htmlEscape(id))                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                //nolint:errcheck
	if len(stageRows) > 0 {
		fmt.Fprint(w, "<div class=\"panel\"><h3>Stages</h3><table><thead><tr><th>stage</th><th>time</th><th>delta_ms</th></tr></thead><tbody>") //nolint:errcheck
		for _, row := range stageRows {
			fmt.Fprintf(w, "<tr><td>%s</td><td><code>%s</code></td><td><code>%s</code></td></tr>", htmlEscape(row[0]), htmlEscape(row[1]), htmlEscape(row[2])) //nolint:errcheck
		}
		fmt.Fprint(w, "</tbody></table></div>") //nolint:errcheck
	} else {
		fmt.Fprint(w, "<div class=\"panel\"><h3>Stages</h3><div class=\"err\" style=\"background:transparent;border:1px dashed #30363d;color:#8b949e;text-align:center;padding:32px;\">No stages recorded yet.</div></div>") //nolint:errcheck
	}
	if lastErrCode != "" {
		fmt.Fprintf(w, "<div class=\"panel\"><h3>Error</h3><div class=\"err\"><strong>%s</strong>: %s</div>", htmlEscape(lastErrCode), htmlEscape(lastErrMsg)) //nolint:errcheck
		if hint != "" {
			fmt.Fprintf(w, "<div class=\"hint\">%s</div>", htmlEscape(hint)) //nolint:errcheck
		}
		fmt.Fprint(w, "</div>") //nolint:errcheck
	}
	if stats != nil {
		b, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Fprintf(w, "<div class=\"panel\"><h3>Stats</h3><pre>%s</pre></div>", htmlEscape(string(b))) //nolint:errcheck
	}
	fmt.Fprint(w, "</body></html>") //nolint:errcheck
}

func errorHintForCode(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "RUNTIME_NOT_CONFIGURED":
		return "在 worker-config.yaml 的 runtimes 下添加该 runtime 配置"
	case "RUNTIME_IMAGE_EMPTY":
		return "为该 runtime 设置镜像名"
	case "EXEC_COMMAND_EMPTY":
		return "为该 runtime 配置可执行命令"
	case "DOCKER_RUN_FAILED":
		return "检查镜像是否存在与启动命令"
	case "DOCKER_WAIT_FAILED":
		return "容器异常退出，查看 agent.stderr.log"
	case "NETWORK_ALLOWLIST_EMPTY":
		return "在 worker-config.yaml 配置 network_allowlist"
	case "NETWORK_ALLOWLIST_INVALID":
		return "修正 allowlist 规则格式"
	case "DOCKER_NETWORK_CREATE_FAILED":
		return "检查 Docker 权限与 network 名称冲突"
	case "DOCKER_NET_POLICY_FAILED":
		return "检查策略镜像与网络连接"
	default:
		return ""
	}
}
