package worker

import (
	"testing"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

func TestDockerExtraArgsFromPolicy_Nil(t *testing.T) {
	got := DockerExtraArgsFromPolicy(nil)
	if got != nil && len(got) != 0 {
		t.Fatalf("expected nil/empty, got=%v", got)
	}
}

func TestDockerExtraArgsFromPolicy_ResourcesAndNetwork(t *testing.T) {
	pol := &runtimev1.Policy{
		Network: runtimev1.NetworkPolicy_NETWORK_POLICY_NONE,
		Resources: &runtimev1.Capacity{
			CpuMillis:   500,
			MemoryBytes: 128 * 1024 * 1024,
			Pids:        64,
		},
	}
	got := DockerExtraArgsFromPolicy(pol)
	want := []string{
		"--network", "none",
		"--cpus", "0.5",
		"--memory", "128m",
		"--pids-limit", "64",
	}
	if len(got) != len(want) {
		t.Fatalf("len mismatch got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx=%d got=%q want=%q full=%v", i, got[i], want[i], got)
		}
	}
}

func TestDockerExtraArgsFromPolicy_CPUFormatting(t *testing.T) {
	pol := &runtimev1.Policy{Resources: &runtimev1.Capacity{CpuMillis: 1250}}
	got := DockerExtraArgsFromPolicy(pol)
	want := []string{"--cpus", "1.25"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx=%d got=%q want=%q full=%v", i, got[i], want[i], got)
		}
	}
}
