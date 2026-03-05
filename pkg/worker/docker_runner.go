package worker

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

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

type DockerProcess struct {
	ContainerID string
	Container   string
	mu          sync.Mutex
}

func DockerNetworkCreateInternal(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "network", "create", "--internal", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "already exists") {
			return nil
		}
		return fmt.Errorf("docker network create failed: %w (%s)", err, msg)
	}
	return nil
}

func DockerNetworkRemove(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "network", "rm", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "No such network") || strings.Contains(msg, "not found") {
			return nil
		}
		return fmt.Errorf("docker network rm failed: %w (%s)", err, msg)
	}
	return nil
}

func DockerNetworkConnect(ctx context.Context, network string, container string) error {
	if strings.TrimSpace(network) == "" || strings.TrimSpace(container) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "network", "connect", network, container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "already exists") || strings.Contains(msg, "already connected") {
			return nil
		}
		return fmt.Errorf("docker network connect failed: %w (%s)", err, msg)
	}
	return nil
}

func DockerExtraArgsFromPolicy(pol *runtimev1.Policy) []string {
	if pol == nil {
		return nil
	}
	args := make([]string, 0, 12)
	switch pol.Network {
	case runtimev1.NetworkPolicy_NETWORK_POLICY_NONE:
		args = append(args, "--network", "none")
	case runtimev1.NetworkPolicy_NETWORK_POLICY_ALL:
	default:
	}

	res := pol.Resources
	if res == nil {
		return args
	}
	if res.CpuMillis > 0 {
		args = append(args, "--cpus", formatDockerCPUs(res.CpuMillis))
	}
	if res.MemoryBytes > 0 {
		args = append(args, "--memory", formatDockerMemory(res.MemoryBytes))
	}
	if res.Pids > 0 {
		args = append(args, "--pids-limit", fmt.Sprintf("%d", res.Pids))
	}
	return args
}

func formatDockerCPUs(cpuMillis uint32) string {
	cpus := float64(cpuMillis) / 1000.0
	s := fmt.Sprintf("%.3f", cpus)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

func formatDockerMemory(memoryBytes uint64) string {
	const mib = uint64(1024 * 1024)
	m := (memoryBytes + mib - 1) / mib
	if m == 0 {
		m = 1
	}
	return fmt.Sprintf("%dm", m)
}

func (p *DockerProcess) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ContainerID == "" && p.Container == "" {
		return nil
	}
	target := p.ContainerID
	if target == "" {
		target = p.Container
	}
	cmd := exec.CommandContext(ctx, "docker", "stop", "-t", "1", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker stop failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func DockerRun(ctx context.Context, spec DockerRunSpec) (*DockerProcess, error) {
	args := []string{"run"}
	if spec.Detach {
		args = append(args, "-d")
	}
	if spec.Remove {
		args = append(args, "--rm")
	}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if spec.Workdir != "" {
		args = append(args, "-w", spec.Workdir)
	}
	for host, container := range spec.Mounts {
		args = append(args, "-v", fmt.Sprintf("%s:%s", host, container))
	}
	for k, v := range spec.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, spec.ExtraArgs...)
	args = append(args, spec.Image)
	args = append(args, spec.Command...)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if spec.Name != "" && strings.Contains(msg, "Conflict. The container name") {
			_ = DockerRemove(ctx, spec.Name)
			cmd2 := exec.CommandContext(ctx, "docker", args...)
			out2, err2 := cmd2.CombinedOutput()
			if err2 != nil {
				return nil, fmt.Errorf("docker run failed: %w (%s)", err2, strings.TrimSpace(string(out2)))
			}
			id2 := strings.TrimSpace(string(out2))
			return &DockerProcess{ContainerID: id2, Container: spec.Name}, nil
		}
		return nil, fmt.Errorf("docker run failed: %w (%s)", err, msg)
	}
	id := strings.TrimSpace(string(out))
	return &DockerProcess{ContainerID: id, Container: spec.Name}, nil
}

type DockerLogStream struct {
	cmd *exec.Cmd
}

func (s *DockerLogStream) Stop() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Kill()
}

func DockerLogsFollow(ctx context.Context, container string, onLine func(isStderr bool, line string)) (*DockerLogStream, error) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", "--since", "0s", container)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			onLine(false, sc.Text())
		}
		if err := sc.Err(); err != nil {
			onLine(true, fmt.Sprintf("stdout scan failed: %v", err))
		}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			onLine(true, sc.Text())
		}
		if err := sc.Err(); err != nil {
			onLine(true, fmt.Sprintf("stderr scan failed: %v", err))
		}
	}()

	go func() {
		_ = cmd.Wait()
	}()

	return &DockerLogStream{cmd: cmd}, nil
}

func DockerWait(ctx context.Context, container string) (int, error) {
	cmd := exec.CommandContext(ctx, "docker", "wait", container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker wait failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, nil
	}
	var code int
	_, scanErr := fmt.Sscanf(s, "%d", &code)
	if scanErr != nil {
		return 0, fmt.Errorf("docker wait invalid output: %s", s)
	}
	return code, nil
}

func DockerRemove(ctx context.Context, container string) error {
	if strings.TrimSpace(container) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker rm failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
