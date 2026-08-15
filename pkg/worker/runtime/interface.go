package runtime

import (
	"context"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

// DockerRunSpec defines the specification for running a Docker container
type DockerRunSpec struct {
	Image     string
	Name      string
	Workdir   string
	Mounts    map[string]string
	Env       map[string]string
	Command   []string
	Detach    bool
	Remove    bool
	ExtraArgs []string
}

// RuntimeAdapter defines the interface for different agent runtimes
type RuntimeAdapter interface { //nolint:revive
	// Name returns the unique name of the runtime adapter (e.g. "nano_agent", "claude_code")
	Name() string

	// BuildSpec generates the Docker run specification for a given request
	BuildSpec(ctx context.Context, req *runtimev1.RunRequest, ctxInfo ContextInfo) (*DockerRunSpec, error)
}

// ContextInfo provides contextual information from the worker to the adapter
type ContextInfo struct {
	StreamID          string
	WorkspaceDir      string
	HostWorkspacePath string // Path on the Docker host (for sibling containers)
	AgentConfigPath   string // Absolute path on the host to the agent config file
	AgentConfigDest   string // Target path inside container for the agent config file
	WorkerID          string
}
