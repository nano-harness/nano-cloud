package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

type NanoAgentAdapter struct { //nolint:revive
	BaseAdapter
	EnvFile string
}

func (a *NanoAgentAdapter) Name() string { //nolint:revive
	return "nano_agent"
}

func (a *NanoAgentAdapter) BuildSpec(ctx context.Context, req *runtimev1.RunRequest, info ContextInfo) (*DockerRunSpec, error) { //nolint:revive
	spec, err := a.BaseAdapter.BuildSpec(ctx, req, info)
	if err != nil {
		return nil, err
	}

	// Load env file if configured
	if a.EnvFile != "" {
		if envs, err := loadEnvFile(a.EnvFile); err == nil {
			for k, v := range envs {
				// Don't overwrite existing envs (e.g. from request or base config)
				if _, ok := spec.Env[k]; !ok {
					spec.Env[k] = v
				}
			}
		} else {
			// Log warning but continue? For now we just return error if file is specified but missing
			// Or maybe we treat it as optional if it doesn't exist?
			// Let's assume if it's configured, it should exist.
			// But to be user friendly, we can check if file exists first.
			if _, statErr := os.Stat(a.EnvFile); statErr == nil {
				return nil, fmt.Errorf("failed to load env file %s: %w", a.EnvFile, err)
			}
		}
	}

	return spec, nil
}

func loadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	envs := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Simple unquote if needed
			if strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`) {
				val = strings.Trim(val, `"`)
			}
			if key != "" {
				envs[key] = val
			}
		}
	}
	return envs, scanner.Err()
}
