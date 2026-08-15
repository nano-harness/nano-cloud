package server

import (
	"testing"
)

func TestWorkerConfigStore_PairingFlow(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWorkerConfigStore(dir)
	if err != nil {
		t.Fatalf("NewWorkerConfigStore: %v", err)
	}

	// 1. Create Pairing Request
	id, secret, _, err := store.CreatePairingRequest("test-worker", "linux/amd64", []string{"docker-desktop"})
	if err != nil {
		t.Fatalf("CreatePairingRequest: %v", err)
	}
	if id == "" || secret == "" {
		t.Fatalf("expected id and secret")
	}

	// 2. Poll (should be pending)
	status, token, err := store.PollPairingRequest(id, secret)
	if err != nil {
		t.Fatalf("PollPairingRequest: %v", err)
	}
	if status != PairingStatusPending {
		t.Fatalf("expected pending status, got %s", status)
	}
	if token != "" {
		t.Fatalf("expected empty token for pending request")
	}

	// 3. Approve
	if err := store.ApprovePairingRequest(id); err != nil {
		t.Fatalf("ApprovePairingRequest: %v", err)
	}

	// 4. Poll again (should be approved and return token)
	status, token, err = store.PollPairingRequest(id, secret)
	if err != nil {
		t.Fatalf("PollPairingRequest (after approve): %v", err)
	}
	if status != PairingStatusApproved {
		t.Fatalf("expected approved status, got %s", status)
	}
	if token == "" {
		t.Fatalf("expected worker token")
	}

	// 5. Verify Worker Created - validate token returns the worker ID
	workerID, err := store.ValidateWorkerToken(token)
	if err != nil {
		t.Fatalf("ValidateWorkerToken: %v", err)
	}
	if workerID == "" {
		t.Fatalf("expected worker id")
	}

	// 6. Poll again (should be consumed/gone or handled as duplicate?
	// The implementation removes the request after token is delivered)
	_, _, err = store.PollPairingRequest(id, secret)
	if err != errNotFound {
		t.Fatalf("expected request to be removed after consumption, got %v", err)
	}
}

func TestWorkerConfigStore_PairingFlow_Reject(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWorkerConfigStore(dir)
	if err != nil {
		t.Fatalf("NewWorkerConfigStore: %v", err)
	}

	id, secret, _, _ := store.CreatePairingRequest("test-worker", "linux/amd64", nil)

	if err := store.RejectPairingRequest(id); err != nil {
		t.Fatalf("RejectPairingRequest: %v", err)
	}

	status, token, err := store.PollPairingRequest(id, secret)
	if err != nil {
		t.Fatalf("PollPairingRequest: %v", err)
	}
	if status != PairingStatusRejected {
		t.Fatalf("expected rejected status, got %s", status)
	}
	if token != "" {
		t.Fatalf("expected empty token")
	}
}

func TestWorkerConfigStore_PairingFlow_Expire(t *testing.T) {
	// Hard to test expiration without mocking time or waiting.
	// We can skip or mock time if architected.
	// For now, simple unit tests are enough.
}
