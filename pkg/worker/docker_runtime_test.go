package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerRuntimeArgs_Empty(t *testing.T) {
	for _, in := range []string{"", "  ", "\t"} {
		got := DockerRuntimeArgs(in)
		if len(got) != 0 {
			t.Fatalf("input=%q expected no args, got=%v", in, got)
		}
	}
}

func TestDockerRuntimeArgs_Runsc(t *testing.T) {
	got := DockerRuntimeArgs("runsc")
	want := []string{"--runtime=runsc"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx=%d got=%q want=%q full=%v", i, got[i], want[i], got)
		}
	}
}

func TestDockerRuntimeArgs_TrimAndPassthrough(t *testing.T) {
	got := DockerRuntimeArgs("  kata  ")
	want := []string{"--runtime=kata"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx=%d got=%q want=%q full=%v", i, got[i], want[i], got)
		}
	}
}

func TestContainerRuntimeErrorHint(t *testing.T) {
	tests := []struct {
		name            string
		msg             string
		containerRuntime string
		wantHint        bool
	}{
		{name: "no runtime configured", msg: "docker run failed: unknown or invalid runtime name: runsc", containerRuntime: "", wantHint: false},
		{name: "runtime error gets hint", msg: "docker run failed: unknown or invalid runtime name: runsc", containerRuntime: "runsc", wantHint: true},
		{name: "unrelated error no hint", msg: "docker run failed: unable to find image locally", containerRuntime: "runsc", wantHint: false},
		{name: "whitespace runtime no hint", msg: "docker run failed: unknown or invalid runtime name: runsc", containerRuntime: "  ", wantHint: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainerRuntimeErrorHint(tt.msg, tt.containerRuntime)
			hasHint := strings.Contains(got, "container_runtime=")
			if hasHint != tt.wantHint {
				t.Fatalf("got=%q wantHint=%v", got, tt.wantHint)
			}
			if !strings.HasPrefix(got, tt.msg) {
				t.Fatalf("original message not preserved: got=%q msg=%q", got, tt.msg)
			}
		})
	}
}

func TestConfigContainerRuntimeYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker-config.yaml")
	content := []byte("relay_url: ws://localhost:8081\ncontainer_runtime: runsc\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ContainerRuntime != "runsc" {
		t.Fatalf("got=%q want=%q", cfg.ContainerRuntime, "runsc")
	}
}
