package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestRunEventsSSE_WithMockLLMStream(t *testing.T) {
	t.Parallel()

	expectedText := "你好，SSE世界"
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		chunks := []string{"你好", "，SSE", "世界"}
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer mockLLM.Close()

	token := "dev-token"
	srv := NewGatewayServerWithLogger(":0", token, t.TempDir(), logrus.New())
	gateway := httptest.NewServer(srv.router)
	defer gateway.Close()

	workerID, workerToken := pairWorkerAndResolveID(t, gateway.URL, token)
	if workerToken == "" {
		t.Fatalf("expected non-empty worker token")
	}
	prompt := "请输出SSE响应"
	runID := createQueuedRun(t, gateway.URL, token, workerID, prompt)

	sseCtx, cancelSSE := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSSE()
	eventsCh := make(chan []*runtimev1.Envelope, 1)
	errCh := make(chan error, 1)

	go func() {
		events, err := collectRunEvents(sseCtx, gateway.URL, token, runID)
		if err != nil {
			errCh <- err
			return
		}
		eventsCh <- events
	}()

	pollReq := workerPollRequest{
		WorkerID:    workerID,
		LastSeq:     0,
		MaxMessages: 1,
	}
	pollBody, err := json.Marshal(pollReq)
	if err != nil {
		t.Fatalf("marshal poll request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/worker/poll", bytes.NewReader(pollBody))
	if err != nil {
		t.Fatalf("build worker poll request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("worker poll failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("worker poll status=%d body=%s", resp.StatusCode, string(raw))
	}
	var pollResp workerPollResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&pollResp)
	if decodeErr != nil {
		t.Fatalf("decode worker poll response: %v", decodeErr)
	}
	if len(pollResp.Messages) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(pollResp.Messages))
	}

	var queuedEnv runtimev1.Envelope
	unmarshalErr := protojson.Unmarshal(pollResp.Messages[0], &queuedEnv)
	if unmarshalErr != nil {
		t.Fatalf("unmarshal queued envelope: %v", unmarshalErr)
	}
	if queuedEnv.GetRunRequest() == nil {
		t.Fatalf("expected run_request envelope, got %T", queuedEnv.Message)
	}
	if queuedEnv.StreamId != runID {
		t.Fatalf("stream_id mismatch, got=%s want=%s", queuedEnv.StreamId, runID)
	}
	if len(queuedEnv.GetRunRequest().GetInput().GetMessages()) == 0 {
		t.Fatalf("expected non-empty input messages")
	}
	gotPrompt := queuedEnv.GetRunRequest().GetInput().GetMessages()[0].GetContent()
	if gotPrompt != prompt {
		t.Fatalf("prompt mismatch, got=%q want=%q", gotPrompt, prompt)
	}

	deltas, err := requestMockLLMAndCollectDeltas(mockLLM.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("collect deltas from mock llm: %v", err)
	}
	if strings.Join(deltas, "") != expectedText {
		t.Fatalf("mock llm text mismatch, got=%q", strings.Join(deltas, ""))
	}

	eventsPayload := make([]json.RawMessage, 0, len(deltas)+1)
	marshaler := protojson.MarshalOptions{UseProtoNames: true}
	for idx, text := range deltas {
		env := &runtimev1.Envelope{
			ProtocolVersion: "1.0",
			WorkerId:        workerID,
			StreamId:        runID,
			Seq:             uint64(idx + 1),
			Message: &runtimev1.Envelope_RunEvent{
				RunEvent: &runtimev1.RunEvent{
					Kind: runtimev1.EventKind_EVENT_KIND_ASSISTANT_DELTA,
					Payload: &runtimev1.RunEvent_AssistantDelta{
						AssistantDelta: &runtimev1.AssistantDeltaEvent{Text: text},
					},
				},
			},
		}
		eventRaw, marshalErr := marshaler.Marshal(env)
		if marshalErr != nil {
			t.Fatalf("marshal assistant_delta event: %v", marshalErr)
		}
		eventsPayload = append(eventsPayload, eventRaw)
	}
	completedEnv := &runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        workerID,
		StreamId:        runID,
		Seq:             uint64(len(deltas) + 1),
		Message: &runtimev1.Envelope_RunEvent{
			RunEvent: &runtimev1.RunEvent{
				Kind: runtimev1.EventKind_EVENT_KIND_COMPLETED,
				Payload: &runtimev1.RunEvent_Completed{
					Completed: &runtimev1.CompletedEvent{
						Success:  true,
						ExitCode: 0,
					},
				},
			},
		},
	}
	completedRaw, err := marshaler.Marshal(completedEnv)
	if err != nil {
		t.Fatalf("marshal completed event: %v", err)
	}
	eventsPayload = append(eventsPayload, completedRaw)

	pushReqBody, err := json.Marshal(workerEventsRequest{
		WorkerID: workerID,
		Messages: eventsPayload,
	})
	if err != nil {
		t.Fatalf("marshal worker events request: %v", err)
	}
	pushReq, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/worker/events", bytes.NewReader(pushReqBody))
	if err != nil {
		t.Fatalf("build worker events request: %v", err)
	}
	pushReq.Header.Set("Authorization", "Bearer "+token)
	pushReq.Header.Set("Content-Type", "application/json")
	pushResp, err := http.DefaultClient.Do(pushReq)
	if err != nil {
		t.Fatalf("post worker events failed: %v", err)
	}
	defer pushResp.Body.Close()
	if pushResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(pushResp.Body)
		t.Fatalf("worker events status=%d body=%s", pushResp.StatusCode, string(raw))
	}

	select {
	case err := <-errCh:
		t.Fatalf("collect sse events failed: %v", err)
	case events := <-eventsCh:
		if len(events) < len(deltas)+1 {
			t.Fatalf("expected at least %d events, got %d", len(deltas)+1, len(events))
		}
		var gotText strings.Builder
		completed := false
		for _, env := range events {
			evt := env.GetRunEvent()
			if evt == nil {
				continue
			}
			if evt.Kind == runtimev1.EventKind_EVENT_KIND_ASSISTANT_DELTA && evt.GetAssistantDelta() != nil {
				gotText.WriteString(evt.GetAssistantDelta().Text)
			}
			if evt.Kind == runtimev1.EventKind_EVENT_KIND_COMPLETED && evt.GetCompleted() != nil {
				completed = evt.GetCompleted().Success
			}
		}
		if gotText.String() != expectedText {
			t.Fatalf("assistant delta text mismatch, got=%q want=%q", gotText.String(), expectedText)
		}
		if !completed {
			t.Fatalf("expected successful completed event")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting sse events")
	}
}

func createQueuedRun(t *testing.T, gatewayURL string, token string, workerID string, prompt string) string {
	t.Helper()
	reqBody, err := json.Marshal(createRunRequest{
		Runtime:  "nano_agent",
		Prompt:   prompt,
		WorkerID: workerID,
	})
	if err != nil {
		t.Fatalf("marshal create run request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/runs", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build create run request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create run failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create run status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out createRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create run response: %v", err)
	}
	if out.RunID == "" {
		t.Fatalf("expected run_id")
	}
	return out.RunID
}

func pairWorkerAndResolveID(t *testing.T, gatewayURL string, token string) (string, string) {
	t.Helper()

	startBody, err := json.Marshal(pairingStartRequest{
		WorkerName: "e2e-worker",
		HostInfo:   "darwin/arm64",
		Labels:     []string{"e2e"},
	})
	if err != nil {
		t.Fatalf("marshal pairing start request: %v", err)
	}
	startReq, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/worker/pairing", bytes.NewReader(startBody))
	if err != nil {
		t.Fatalf("build pairing start request: %v", err)
	}
	startReq.Header.Set("Content-Type", "application/json")
	startResp, err := http.DefaultClient.Do(startReq)
	if err != nil {
		t.Fatalf("start pairing failed: %v", err)
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(startResp.Body)
		t.Fatalf("start pairing status=%d body=%s", startResp.StatusCode, string(raw))
	}
	var startOut pairingStartResponse
	startDecodeErr := json.NewDecoder(startResp.Body).Decode(&startOut)
	if startDecodeErr != nil {
		t.Fatalf("decode pairing start response: %v", startDecodeErr)
	}
	if startOut.ID == "" || startOut.Secret == "" {
		t.Fatalf("expected pairing id and secret")
	}

	pendingReq, err := http.NewRequest(http.MethodGet, gatewayURL+"/v1/worker/pairing/"+startOut.ID, nil)
	if err != nil {
		t.Fatalf("build pairing pending request: %v", err)
	}
	pendingReq.Header.Set("Authorization", "Bearer "+startOut.Secret)
	pendingResp, err := http.DefaultClient.Do(pendingReq)
	if err != nil {
		t.Fatalf("poll pairing pending failed: %v", err)
	}
	defer pendingResp.Body.Close()
	if pendingResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(pendingResp.Body)
		t.Fatalf("poll pending status=%d body=%s", pendingResp.StatusCode, string(raw))
	}
	var pendingOut pairingStatusResponse
	pendingDecodeErr := json.NewDecoder(pendingResp.Body).Decode(&pendingOut)
	if pendingDecodeErr != nil {
		t.Fatalf("decode pairing pending response: %v", pendingDecodeErr)
	}
	if pendingOut.Status != PairingStatusPending {
		t.Fatalf("pairing status mismatch before approve, got=%s", pendingOut.Status)
	}

	approveReq, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/admin/pairing/"+startOut.ID+"/approve", nil)
	if err != nil {
		t.Fatalf("build approve pairing request: %v", err)
	}
	approveReq.Header.Set("Authorization", "Bearer "+token)
	approveResp, err := http.DefaultClient.Do(approveReq)
	if err != nil {
		t.Fatalf("approve pairing failed: %v", err)
	}
	defer approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(approveResp.Body)
		t.Fatalf("approve pairing status=%d body=%s", approveResp.StatusCode, string(raw))
	}

	approvedReq, err := http.NewRequest(http.MethodGet, gatewayURL+"/v1/worker/pairing/"+startOut.ID, nil)
	if err != nil {
		t.Fatalf("build pairing approved request: %v", err)
	}
	approvedReq.Header.Set("Authorization", "Bearer "+startOut.Secret)
	approvedResp, err := http.DefaultClient.Do(approvedReq)
	if err != nil {
		t.Fatalf("poll pairing approved failed: %v", err)
	}
	defer approvedResp.Body.Close()
	if approvedResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(approvedResp.Body)
		t.Fatalf("poll approved status=%d body=%s", approvedResp.StatusCode, string(raw))
	}
	var approvedOut pairingStatusResponse
	approvedDecodeErr := json.NewDecoder(approvedResp.Body).Decode(&approvedOut)
	if approvedDecodeErr != nil {
		t.Fatalf("decode pairing approved response: %v", approvedDecodeErr)
	}
	if approvedOut.Status != PairingStatusApproved {
		t.Fatalf("pairing status mismatch after approve, got=%s", approvedOut.Status)
	}
	if approvedOut.WorkerToken == "" {
		t.Fatalf("expected worker token after approve")
	}

	// Resolve worker ID via admin list workers API
	listReq, err := http.NewRequest(http.MethodGet, gatewayURL+"/v1/admin/workers", nil)
	if err != nil {
		t.Fatalf("build admin list workers request: %v", err)
	}
	listReq.Header.Set("Authorization", "Bearer "+token)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("admin list workers failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(listResp.Body)
		t.Fatalf("admin list workers status=%d body=%s", listResp.StatusCode, string(raw))
	}
	var workers []struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&workers); err != nil {
		t.Fatalf("decode admin list workers response: %v", err)
	}
	if len(workers) == 0 {
		t.Fatalf("expected at least one worker")
	}
	return workers[0].WorkerID, approvedOut.WorkerToken
}

func requestMockLLMAndCollectDeltas(url string) ([]string, error) {
	reqBody := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(raw))
	}

	type mockChunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	out := make([]string, 0, 8)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk mockChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, err
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		text := chunk.Choices[0].Delta.Content
		if text != "" {
			out = append(out, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func collectRunEvents(ctx context.Context, gatewayURL string, token string, runID string) ([]*runtimev1.Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/v1/runs/"+runID+"/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	events := make([]*runtimev1.Envelope, 0, 16)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var env runtimev1.Envelope
		if err := protojson.Unmarshal([]byte(data), &env); err != nil {
			return nil, err
		}
		events = append(events, &env)
		if evt := env.GetRunEvent(); evt != nil && evt.Kind == runtimev1.EventKind_EVENT_KIND_COMPLETED {
			return events, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func TestSSE_ReconnectAndHistory(t *testing.T) {
	t.Parallel()

	token := "dev-token"
	srv := NewGatewayServerWithLogger(":0", token, t.TempDir(), logrus.New())
	gateway := httptest.NewServer(srv.router)
	defer gateway.Close()

	workerID, _ := pairWorkerAndResolveID(t, gateway.URL, token)
	runID := createQueuedRun(t, gateway.URL, token, workerID, "test reconnect")

	// Push 3 events
	marshaler := protojson.MarshalOptions{UseProtoNames: true}
	var eventsPayload []json.RawMessage
	for i := 1; i <= 3; i++ {
		env := &runtimev1.Envelope{
			ProtocolVersion: "1.0",
			WorkerId:        workerID,
			StreamId:        runID,
			Seq:             uint64(i),
			Message: &runtimev1.Envelope_RunEvent{
				RunEvent: &runtimev1.RunEvent{
					Kind: runtimev1.EventKind_EVENT_KIND_ASSISTANT_DELTA,
					Payload: &runtimev1.RunEvent_AssistantDelta{
						AssistantDelta: &runtimev1.AssistantDeltaEvent{Text: fmt.Sprintf("chunk-%d", i)},
					},
				},
			},
		}
		raw, _ := marshaler.Marshal(env)
		eventsPayload = append(eventsPayload, raw)
	}
	pushReqBody, _ := json.Marshal(workerEventsRequest{WorkerID: workerID, Messages: eventsPayload})
	pushReq, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/worker/events", bytes.NewReader(pushReqBody))
	pushReq.Header.Set("Authorization", "Bearer "+token)
	pushReq.Header.Set("Content-Type", "application/json")
	pushResp, _ := http.DefaultClient.Do(pushReq)
	pushResp.Body.Close()

	// Connect SSE and read events, simulating a disconnect after 3 events
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() {
		// Wait a tiny bit then cancel to simulate disconnect
		time.Sleep(500 * time.Millisecond)
		cancel1()
	}()
	events1, err := collectRunEvents(ctx1, gateway.URL, token, runID)
	_ = events1
	_ = err

	// Push COMPLETED event
	compEnv := &runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        workerID,
		StreamId:        runID,
		Seq:             4,
		Message: &runtimev1.Envelope_RunEvent{
			RunEvent: &runtimev1.RunEvent{
				Kind: runtimev1.EventKind_EVENT_KIND_COMPLETED,
				Payload: &runtimev1.RunEvent_Completed{
					Completed: &runtimev1.CompletedEvent{Success: true},
				},
			},
		},
	}
	compRaw, _ := marshaler.Marshal(compEnv)
	pushReqBody2, _ := json.Marshal(workerEventsRequest{WorkerID: workerID, Messages: []json.RawMessage{compRaw}})
	pushReq2, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/worker/events", bytes.NewReader(pushReqBody2))
	pushReq2.Header.Set("Authorization", "Bearer "+token)
	pushReq2.Header.Set("Content-Type", "application/json")
	pushResp2, _ := http.DefaultClient.Do(pushReq2)
	pushResp2.Body.Close()

	// Reconnect SSE, we should get ALL 4 events from history
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	events2, err2 := collectRunEvents(ctx2, gateway.URL, token, runID)
	if err2 != nil {
		t.Fatalf("reconnect collect failed: %v", err2)
	}

	if len(events2) != 4 {
		t.Fatalf("expected 4 events on reconnect, got %d", len(events2))
	}
	gotChunks := ""
	for _, e := range events2 {
		if e.GetRunEvent().GetAssistantDelta() != nil {
			gotChunks += e.GetRunEvent().GetAssistantDelta().Text
		}
	}
	if gotChunks != "chunk-1chunk-2chunk-3" {
		t.Fatalf("unexpected chunks: %s", gotChunks)
	}
}

func TestWorker_WebSocketReconnect(t *testing.T) {
	t.Parallel()

	token := "dev-token"
	srv := NewGatewayServerWithLogger(":0", token, t.TempDir(), logrus.New())
	gateway := httptest.NewServer(srv.router)
	defer gateway.Close()

	// Connect first time
	workerID, workerToken := pairWorkerAndResolveID(t, gateway.URL, token)

	wsURL := "ws" + strings.TrimPrefix(gateway.URL, "http") + "/v1/worker/connect"
	req1, _ := http.NewRequest("GET", wsURL, nil)
	req1.Header.Set("Authorization", "Bearer "+workerToken)

	conn1, resp1, err := websocket.DefaultDialer.Dial(wsURL, req1.Header)
	if err != nil {
		t.Fatalf("first connect failed: %v", err)
	}
	defer conn1.Close()
	if resp1.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("first connect status: %d", resp1.StatusCode)
	}

	helloEnv1 := &runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        workerID,
		Seq:             1,
		Message: &runtimev1.Envelope_WorkerHello{
			WorkerHello: &runtimev1.WorkerHello{
				Name:    "worker",
				Version: "1.0",
			},
		},
	}
	raw1, _ := proto.Marshal(helloEnv1)
	conn1.WriteMessage(websocket.BinaryMessage, raw1)

	time.Sleep(100 * time.Millisecond)

	// Verify worker is online
	srv.mu.RLock()
	_, online1 := srv.workers[workerID]
	srv.mu.RUnlock()
	if !online1 {
		t.Fatalf("worker should be online")
	}

	// Force disconnect worker from server side by closing the websocket
	srv.mu.Lock()
	sess := srv.workers[workerID]
	if sess != nil && sess.Conn != nil {
		sess.Conn.Close()
	}
	srv.mu.Unlock()

	// Wait a moment for gateway to detect close
	time.Sleep(100 * time.Millisecond)

	// Reconnect worker
	req2, _ := http.NewRequest("GET", wsURL, nil)
	req2.Header.Set("Authorization", "Bearer "+workerToken)

	conn2, resp2, err := websocket.DefaultDialer.Dial(wsURL, req2.Header)
	if err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	defer conn2.Close()
	if resp2.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("reconnect status: %d", resp2.StatusCode)
	}

	// Send Hello again
	helloEnv2 := &runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        workerID,
		Seq:             1,
		Message: &runtimev1.Envelope_WorkerHello{
			WorkerHello: &runtimev1.WorkerHello{
				Name:    "reconnected-worker",
				Version: "1.0",
			},
		},
	}
	raw2, err := proto.Marshal(helloEnv2)
	if err != nil {
		t.Fatalf("marshal hello failed: %v", err)
	}
	if err := conn2.WriteMessage(websocket.BinaryMessage, raw2); err != nil {
		t.Fatalf("write hello failed: %v", err)
	}

	// Wait for server to process hello
	time.Sleep(100 * time.Millisecond)

	// Verify worker is online again
	srv.mu.RLock()
	sess2, online2 := srv.workers[workerID]
	srv.mu.RUnlock()
	if !online2 || sess2 == nil {
		t.Fatalf("worker should be online after reconnect")
	}
}

func TestRunRequest_SessionId(t *testing.T) {
	t.Parallel()

	token := "dev-token"
	srv := NewGatewayServerWithLogger(":0", token, t.TempDir(), logrus.New())
	gateway := httptest.NewServer(srv.router)
	defer gateway.Close()

	workerID, _ := pairWorkerAndResolveID(t, gateway.URL, token)

	prompt := "hello with session"
	sessionID := "test-session-123"

	reqBody, err := json.Marshal(createRunRequest{
		Runtime:   "nano_agent",
		Prompt:    prompt,
		WorkerID:  workerID,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/runs", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create run failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create run status: %d", resp.StatusCode)
	}

	// Fetch queued request for worker
	pollReq := workerPollRequest{
		WorkerID:    workerID,
		LastSeq:     0,
		MaxMessages: 1,
	}
	pollBody, _ := json.Marshal(pollReq)
	preq, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/worker/poll", bytes.NewReader(pollBody))
	preq.Header.Set("Authorization", "Bearer "+token)
	preq.Header.Set("Content-Type", "application/json")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	defer presp.Body.Close()

	var pollResp workerPollResponse
	if err := json.NewDecoder(presp.Body).Decode(&pollResp); err != nil {
		t.Fatalf("decode poll resp: %v", err)
	}

	if len(pollResp.Messages) == 0 {
		t.Fatalf("expected message queued")
	}

	var queuedEnv runtimev1.Envelope
	if err := protojson.Unmarshal(pollResp.Messages[0], &queuedEnv); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}

	runReq := queuedEnv.GetRunRequest()
	if runReq == nil {
		t.Fatalf("expected RunRequest")
	}

	if runReq.SessionId != sessionID {
		t.Fatalf("expected session_id %q, got %q", sessionID, runReq.SessionId)
	}
}
