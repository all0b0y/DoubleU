package httpapi

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// mcpDockerDenyList are the docker-compose service hostnames that resolve to
// internal infrastructure on the compose network. They are rejected even when
// DNS cannot resolve them from the host, so a user-supplied `mcp_servers.url`
// can never point the agent's egress at our own Postgres/Redis/etc.
//
// Kept in sync manually with docker-compose.yml service names; adding a service
// that the agent must never reach means adding it here.
var mcpDockerDenyList = map[string]struct{}{
	"postgres":   {},
	"redis":      {},
	"backend":    {},
	"ai-service": {},
	"litellm":    {},
	"langfuse":   {},
}

// mcpLookupIP is the DNS resolver used by ValidateMCPServerURL. It is a package
// variable purely so tests can stay hermetic (no live network) while still
// exercising every branch; production always uses net.LookupIP.
var mcpLookupIP = net.LookupIP

// ValidateMCPServerURL rejects URLs that could reach internal infrastructure.
// Security assumption: mcp_servers.url becomes user-supplied once the CRUD API
// ships, so every address must resolve to a public unicast IP.
//
// Checks run in order (scheme → DNS → deny-list) so the cheapest, most decisive
// rejections happen first. A hostname that fails to resolve is treated as a
// rejection: the guard fails closed rather than letting an unresolvable (or
// compose-only) name through.
func ValidateMCPServerURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("mcp url is not parseable")
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("mcp url has no host")
	}

	// Deny-list first: docker-network service names must never be reachable and
	// generally do not resolve from the host, so checking them before the DNS
	// lookup keeps the guard deterministic and network-independent for them.
	if _, denied := mcpDockerDenyList[strings.ToLower(host)]; denied {
		return fmt.Errorf("mcp url host is an internal service name")
	}

	// Scheme: only https. http is a dev-stand escape hatch, enabled explicitly
	// via MCP_ALLOW_INSECURE=true — never assume plaintext egress is acceptable
	// in production, but local stands often lack TLS on their MCP mock.
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !boolValue(os.Getenv("MCP_ALLOW_INSECURE")) {
			return fmt.Errorf("mcp url must use https")
		}
	default:
		return fmt.Errorf("mcp url must use https")
	}

	addrs, err := mcpLookupIP(host)
	if err != nil {
		// Fail closed: an unresolvable host cannot be proven public, and it may
		// be a compose-only name not in the static deny-list above.
		return fmt.Errorf("mcp url host does not resolve")
	}
	for _, ip := range addrs {
		if isDisallowedMCPIP(ip) {
			return fmt.Errorf("mcp url resolves to a non-public address")
		}
	}
	return nil
}

// isDisallowedMCPIP reports whether ip belongs to a range the agent must never
// egress to. net.IP.IsPrivate already covers IPv6 ULA (fc00::/7), but the
// fc00::/7 test is asserted explicitly per the security plan so the guarantee
// survives even if that stdlib behaviour ever narrows.
func isDisallowedMCPIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if v6 := ip.To16(); v6 != nil && ip.To4() == nil {
		// IPv6 ULA: fc00::/7 → the top 7 bits are 1111 110.
		if v6[0]&0xfe == 0xfc {
			return true
		}
	}
	return false
}

// boolValue mirrors config.boolValue's truthiness so the escape-hatch env flag
// behaves the same way as every other feature flag. Duplicated (rather than
// imported) to keep the httpapi security boundary free of a config dependency.
func boolValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
