package worker

import (
	"testing"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

func TestValidateNetworkAllowlist_CIDR(t *testing.T) {
	rules := []NetworkAllowRule{
		{CIDR: "1.1.1.0/24", Protocol: "tcp", Ports: []int{443}},
	}
	if err := ValidateNetworkAllowlist(rules); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerExtraArgsFromPolicy_AllowlistDoesNotForceNetwork(t *testing.T) {
	pol := &runtimev1.Policy{Network: runtimev1.NetworkPolicy_NETWORK_POLICY_ALLOWLIST}
	got := DockerExtraArgsFromPolicy(pol)
	for i := 0; i < len(got); i++ {
		if got[i] == "--network" {
			t.Fatalf("unexpected network flag in allowlist policy: %v", got)
		}
	}
}
