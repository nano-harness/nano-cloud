package runtime

import (
	"context"
	"testing"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

func TestBaseAdapter_BuildSpec_SessionId(t *testing.T) {
	adapter := &BaseAdapter{
		Image:   "test-image:latest",
		Command: []string{"test-cmd"},
		Env: map[string]string{
			"BASE_ENV": "1",
		},
	}

	req := &runtimev1.RunRequest{
		SessionId: "sess-12345",
	}

	ctxInfo := ContextInfo{
		StreamID:     "stream-123",
		WorkspaceDir: "/tmp/workspace-dir",
		WorkerID:     "worker-1",
	}

	spec, err := adapter.BuildSpec(context.Background(), req, ctxInfo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.Image != "test-image:latest" {
		t.Errorf("expected image test-image:latest, got %s", spec.Image)
	}

	if val, ok := spec.Env["SESSION_ID"]; !ok || val != "sess-12345" {
		t.Errorf("expected SESSION_ID=sess-12345, got %v", val)
	}

	if val, ok := spec.Env["STREAM_ID"]; !ok || val != "stream-123" {
		t.Errorf("expected STREAM_ID=stream-123, got %v", val)
	}

	if val, ok := spec.Env["BASE_ENV"]; !ok || val != "1" {
		t.Errorf("expected BASE_ENV=1, got %v", val)
	}

	if val, ok := spec.Mounts["/tmp/workspace-dir"]; !ok || val != "/workspace" {
		t.Errorf("expected mount /tmp/workspace-dir:/workspace, got %v", spec.Mounts)
	}
}
