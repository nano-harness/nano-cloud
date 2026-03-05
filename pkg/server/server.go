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
	addr              string
	router            *mux.Router
	workers           map[string]*WorkerSession
	workerQueue       map[string][]*runtimev1.Envelope
	streamEvents      map[string][]*runtimev1.Envelope
	streamLastAt      map[string]time.Time
	streamDoneAt      map[string]time.Time
	streamSubs        map[string]map[chan *runtimev1.Envelope]struct{}
	runs              map[string]string
	seqCounter        uint64
	mu                sync.RWMutex
	logger            *logrus.Logger
	token             string
	configStore       *WorkerConfigStore
	consoleUsername   string
	consolePassword   string
	consoleSessionTTL time.Duration
	consoleSessions   map[string]time.Time
}

const (
	maxWorkerQueuePerWorker  = 1000
	maxStreamEventsPerStream = 2000
	streamRetention          = 10 * time.Minute
)

type WorkerSession struct {
	ID            string
	Conn          *websocket.Conn
	Hello         *runtimev1.WorkerHello
	LastHeartbeat time.Time
	writeMu       sync.Mutex
}

func NewGatewayServer(addr, token, configStoreDir string) *GatewayServer {
	return NewGatewayServerWithLogger(addr, token, configStoreDir, logrus.New())
}

func NewGatewayServerWithLogger(addr, token, configStoreDir string, logger *logrus.Logger) *GatewayServer {
	if logger == nil {
		logger = logrus.New()
	}
	store, err := NewWorkerConfigStore(configStoreDir)
	if err != nil {
		logger.WithError(err).Fatal("failed to initialize worker config store")
	}
	s := &GatewayServer{
		addr:              addr,
		router:            mux.NewRouter(),
		workers:           make(map[string]*WorkerSession),
		workerQueue:       make(map[string][]*runtimev1.Envelope),
		streamEvents:      make(map[string][]*runtimev1.Envelope),
		streamLastAt:      make(map[string]time.Time),
		streamDoneAt:      make(map[string]time.Time),
		streamSubs:        make(map[string]map[chan *runtimev1.Envelope]struct{}),
		runs:              make(map[string]string),
		logger:            logger,
		token:             token,
		configStore:       store,
		consoleUsername:   strings.TrimSpace(os.Getenv("CONSOLE_USERNAME")),
		consolePassword:   os.Getenv("CONSOLE_PASSWORD"),
		consoleSessionTTL: parseConsoleSessionTTL(),
		consoleSessions:   make(map[string]time.Time),
	}
	s.setupRoutes()
	return s
}

func (s *GatewayServer) setupRoutes() {
	s.router.HandleFunc("/v1/worker/connect", s.handleWorkerConnect).Methods("GET")
	s.router.HandleFunc("/v1/worker/poll", s.handleWorkerPoll).Methods("POST")
	s.router.HandleFunc("/v1/worker/events", s.handleWorkerEvents).Methods("POST")
	s.router.HandleFunc("/v1/worker/enroll", s.handleWorkerEnroll).Methods("POST")
	s.router.HandleFunc("/v1/worker/config", s.handleWorkerGetConfig).Methods("GET")
	s.router.HandleFunc("/v1/worker/config/ack", s.handleWorkerConfigAck).Methods("POST")
	s.router.HandleFunc("/v1/admin/enroll-tokens", s.handleAdminCreateEnrollToken).Methods("POST")
	s.router.HandleFunc("/v1/admin/enroll-tokens", s.handleAdminListEnrollTokens).Methods("GET")
	s.router.HandleFunc("/v1/admin/enroll-tokens/{token}/revoke", s.handleAdminRevokeEnrollToken).Methods("POST")
	s.router.HandleFunc("/v1/admin/workers", s.handleAdminListWorkers).Methods("GET")
	s.router.HandleFunc("/v1/admin/workers/{id}/config", s.handleAdminGetWorkerConfig).Methods("GET")
	s.router.HandleFunc("/v1/admin/workers/{id}/config", s.handleAdminPutWorkerConfig).Methods("PUT")
	s.router.HandleFunc("/v1/admin/workers/{id}/rotate-token", s.handleAdminRotateWorkerToken).Methods("POST")
	s.router.HandleFunc("/v1/workers", s.handleListWorkers).Methods("GET")
	s.router.HandleFunc("/v1/runs", s.handleCreateRun).Methods("POST")
	s.router.HandleFunc("/v1/runs/{id}/events", s.handleRunEvents).Methods("GET")
	s.router.HandleFunc("/v1/runs/{id}/cancel", s.handleCancelRun).Methods("POST")
	s.router.HandleFunc("/console", s.handleConsole).Methods("GET")
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

func (s *GatewayServer) Start() error {
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
		}).Info("http")
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
	s.streamLastAt[streamID] = time.Now()
	if len(s.streamEvents[streamID]) > maxStreamEventsPerStream {
		s.streamEvents[streamID] = s.streamEvents[streamID][len(s.streamEvents[streamID])-maxStreamEventsPerStream:]
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

func (s *GatewayServer) DispatchRun(workerID string, streamID string, req *runtimev1.RunRequest) error {
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

func (s *GatewayServer) DispatchCancel(workerID string, streamID string, reason string) error {
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
	defer conn.Close()

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

	for {
		inEnv, err := s.readWorkerEnvelope(conn)
		if err != nil {
			s.logger.Infof("Worker disconnected: %s (%v)", workerID, err)
			break
		}

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
	json.NewEncoder(w).Encode(resp)
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
	json.NewEncoder(w).Encode(workerEventsAck{Accepted: true, Detail: fmt.Sprintf("accepted=%d", accepted)})
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
	json.NewEncoder(w).Encode(list)
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
	json.NewEncoder(w).Encode(createRunResponse{
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
	json.NewEncoder(w).Encode(map[string]any{"accepted": true})
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
		fmt.Fprintf(w, "event: run_event\n")
		fmt.Fprintf(w, "data: %s\n\n", string(b))
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
			fmt.Fprintf(w, "event: ping\ndata: {}\n\n")
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

	configByWorkerID := map[string]workerRecord{}
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

	fmt.Fprint(w, "<!DOCTYPE html><html><head><meta charset=\"utf-8\" /><title>Nano Cloud Console</title>")
	fmt.Fprint(w, "<style>body{font-family:system-ui,sans-serif;padding:16px;background:#0b0f14;color:#e6edf3}h1{margin:0 0 8px 0}h2{margin:18px 0 8px 0;font-size:14px;letter-spacing:1px;color:#9fb0c0}table{border-collapse:collapse;width:100%}th,td{border:1px solid rgba(255,255,255,0.12);padding:8px;font-size:13px}th{background:rgba(255,255,255,0.06);text-align:left}code{font-family:ui-monospace,Menlo,monospace}small{color:#9fb0c0}.ok{color:#7ee787}.bad{color:#ff7b72}.panel{margin-top:16px;border:1px solid rgba(255,255,255,0.12);padding:12px;border-radius:8px;background:rgba(255,255,255,0.03)}label{display:block;margin-top:8px}.field{margin-top:4px;padding:8px;border-radius:6px;border:1px solid rgba(255,255,255,0.2);background:#0f141b;color:#e6edf3}.btn{margin-top:10px;padding:8px 12px;border-radius:6px;border:1px solid rgba(255,255,255,0.25);background:#1f6feb;color:white;cursor:pointer}.btn.secondary{background:#30363d}.err{color:#ff7b72;margin-top:8px}</style>")
	fmt.Fprint(w, "</head><body>")
	fmt.Fprint(w, "<h1>Nano Cloud Console</h1>")
	fmt.Fprintf(w, "<small>server_time=%s | workers_online=%d | runs_tracked=%d</small>", now.Format(time.RFC3339), len(publicWorkers), len(runs))

	if authEnabled && !loggedIn {
		fmt.Fprint(w, "<div class=\"panel\"><h2>LOGIN REQUIRED FOR SENSITIVE DATA</h2>")
		fmt.Fprint(w, "<small>匿名访问仅展示公开概览，敏感字段需要登录。</small>")
		if loginFailed {
			fmt.Fprint(w, "<div class=\"err\">用户名或密码错误</div>")
		}
		fmt.Fprint(w, "<form method=\"post\" action=\"/console/login\">")
		fmt.Fprint(w, "<label>用户名</label><input class=\"field\" type=\"text\" name=\"username\" autocomplete=\"username\" />")
		fmt.Fprint(w, "<label>密码</label><input class=\"field\" type=\"password\" name=\"password\" autocomplete=\"current-password\" />")
		fmt.Fprint(w, "<button class=\"btn\" type=\"submit\">登录</button></form></div>")
	}
	if authEnabled && loggedIn {
		fmt.Fprint(w, "<div class=\"panel\"><small>已登录，可查看敏感信息</small>")
		fmt.Fprint(w, "<form method=\"post\" action=\"/console/logout\"><button class=\"btn secondary\" type=\"submit\">退出登录</button></form></div>")
	}
	if !authEnabled {
		fmt.Fprint(w, "<div class=\"panel\"><small>未配置 CONSOLE_USERNAME / CONSOLE_PASSWORD，敏感信息默认隐藏。</small></div>")
	}

	fmt.Fprint(w, "<h2>PUBLIC WORKERS</h2>")
	fmt.Fprint(w, "<table><thead><tr><th>name</th><th>version</th><th>runtimes</th><th>last_seen</th></tr></thead><tbody>")
	for _, row := range publicWorkers {
		last := time.Unix(row.LastSeenUnixSec, 0).Format(time.RFC3339)
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td><code>%s</code></td></tr>", htmlEscape(row.Name), htmlEscape(row.Version), htmlEscape(row.Supported), last)
	}
	fmt.Fprint(w, "</tbody></table>")

	if !loggedIn {
		fmt.Fprint(w, "</body></html>")
		return
	}

	fmt.Fprint(w, "<h2>PRIVATE WORKERS</h2>")
	fmt.Fprint(w, "<table><thead><tr><th>id</th><th>name</th><th>version</th><th>labels</th><th>runtimes</th><th>config</th><th>applied</th><th>status</th><th>last_seen</th></tr></thead><tbody>")
	for _, row := range workers {
		last := time.Unix(row.LastSeenUnixSec, 0).Format(time.RFC3339)
		statusClass := "bad"
		statusText := "pending"
		if row.ConfigApplied {
			statusClass = "ok"
			statusText = "applied"
		}
		fmt.Fprintf(w, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td><code>%s</code></td><td><code>%s</code></td><td><span class=\"%s\">%s</span></td><td><code>%s</code></td></tr>", htmlEscape(row.IDPrefix), htmlEscape(row.Name), htmlEscape(row.Version), htmlEscape(row.Labels), htmlEscape(row.Supported), htmlEscape(row.ConfigPrefix), htmlEscape(row.AppliedPrefix), statusClass, statusText, last)
	}
	fmt.Fprint(w, "</tbody></table>")

	fmt.Fprint(w, "<h2>RECENT RUNS</h2>")
	fmt.Fprint(w, "<table><thead><tr><th>run</th><th>worker</th><th>events</th><th>last_seq</th></tr></thead><tbody>")
	for _, row := range runs {
		fmt.Fprintf(w, "<tr><td><code>%s</code></td><td><code>%s</code></td><td>%d</td><td><code>%d</code></td></tr>", htmlEscape(row.RunIDPrefix), htmlEscape(row.WorkerIDPrefix), row.EventCount, row.LastSeq)
	}
	fmt.Fprint(w, "</tbody></table>")

	fmt.Fprint(w, "</body></html>")
}

func (s *GatewayServer) consoleAuthEnabled() bool {
	return strings.TrimSpace(s.consoleUsername) != "" && s.consolePassword != ""
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
	exp := time.Now().Add(s.consoleSessionTTL)
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
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.consoleUsername)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.consolePassword)) == 1
	if !userOK || !passOK {
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
