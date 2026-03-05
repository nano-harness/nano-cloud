package worker

import (
	"fmt"
	"net/netip"
	"strings"
)

func ValidateNetworkAllowlist(rules []NetworkAllowRule) error {
	for _, r := range rules {
		proto := strings.ToLower(strings.TrimSpace(r.Protocol))
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" {
			return fmt.Errorf("invalid protocol: %q", r.Protocol)
		}

		ports := r.Ports
		if len(ports) == 0 {
			ports = []int{443}
		}
		for _, p := range ports {
			if p <= 0 || p > 65535 {
				return fmt.Errorf("invalid port: %d", p)
			}
		}

		host := strings.TrimSpace(r.Host)
		cidr := strings.TrimSpace(r.CIDR)
		if host == "" && cidr == "" {
			return fmt.Errorf("allowlist rule requires host or cidr")
		}
		if host != "" && cidr != "" {
			return fmt.Errorf("allowlist rule cannot set both host and cidr")
		}

		if cidr != "" {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return fmt.Errorf("invalid cidr %q: %w", cidr, err)
			}
			if !prefix.Addr().Is4() {
				continue
			}
		}
	}
	return nil
}
