package httpapi

import (
	"errors"
	"net"
	"testing"
)

// stubResolver replaces the package DNS seam for the duration of a test so the
// suite never depends on live internet. Literal-IP URLs never hit it (url host
// is already an address); it only matters for hostname cases.
func stubResolver(t *testing.T, fn func(host string) ([]net.IP, error)) {
	t.Helper()
	prev := mcpLookupIP
	mcpLookupIP = fn
	t.Cleanup(func() { mcpLookupIP = prev })
}

// literalResolver resolves a host to itself when it is already a literal IP and
// fails otherwise. This keeps literal-IP cases hermetic without pretending to
// resolve real hostnames.
func literalResolver(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return nil, errors.New("no such host")
}

func TestMCPServerURL_RejectsInternalAndPrivate(t *testing.T) {
	stubResolver(t, literalResolver)

	rejected := []string{
		"https://postgres:5432",  // docker deny-list
		"https://redis:6379",     // docker deny-list
		"https://ai-service",     // docker deny-list
		"https://litellm:4000",   // docker deny-list
		"https://langfuse",       // docker deny-list
		"https://127.0.0.1",      // loopback
		"https://10.0.0.5",       // private class A
		"https://192.168.1.1",    // private class C
		"https://169.254.1.1",    // link-local unicast
		"https://[::1]",          // IPv6 loopback
		"https://[fd00::1]",      // IPv6 ULA
		"https://0.0.0.0",        // unspecified
		"http://api.example.com", // plaintext without escape hatch
		"https://",               // empty host
		"not a url",              // no host / unparseable-ish
	}
	for _, raw := range rejected {
		if err := ValidateMCPServerURL(raw); err == nil {
			t.Errorf("ValidateMCPServerURL(%q) = nil, want error", raw)
		}
	}
}

func TestMCPServerURL_AllowsPublicHTTPS(t *testing.T) {
	stubResolver(t, literalResolver)

	// Literal public unicast IP → no DNS, fully hermetic happy path.
	if err := ValidateMCPServerURL("https://93.184.216.34/mcp"); err != nil {
		t.Errorf("ValidateMCPServerURL(public IP) = %v, want nil", err)
	}
}

func TestMCPServerURL_AllowsPublicHostname(t *testing.T) {
	// Stub DNS so a public hostname resolves to a public address without network.
	stubResolver(t, func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	if err := ValidateMCPServerURL("https://api.example.com/mcp"); err != nil {
		t.Errorf("ValidateMCPServerURL(public hostname) = %v, want nil", err)
	}
}

func TestMCPServerURL_HTTPRequiresInsecureFlag(t *testing.T) {
	stubResolver(t, func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	// Without the flag http is rejected outright.
	if err := ValidateMCPServerURL("http://api.example.com/mcp"); err == nil {
		t.Error("http://api.example.com without MCP_ALLOW_INSECURE = nil, want error")
	}

	// With the dev-stand escape hatch http passes (public host).
	t.Setenv("MCP_ALLOW_INSECURE", "true")
	if err := ValidateMCPServerURL("http://api.example.com/mcp"); err != nil {
		t.Errorf("http://api.example.com with MCP_ALLOW_INSECURE=true = %v, want nil", err)
	}
}

func TestMCPServerURL_InsecureFlagStillBlocksPrivate(t *testing.T) {
	stubResolver(t, literalResolver)
	t.Setenv("MCP_ALLOW_INSECURE", "true")

	// The escape hatch relaxes the scheme, never the address ranges.
	if err := ValidateMCPServerURL("http://10.0.0.5"); err == nil {
		t.Error("http://10.0.0.5 with MCP_ALLOW_INSECURE=true = nil, want error")
	}
}

func TestMCPServerURL_UnresolvableHostRejected(t *testing.T) {
	stubResolver(t, literalResolver) // rejects any non-literal host
	if err := ValidateMCPServerURL("https://totally-not-real.invalid"); err == nil {
		t.Error("unresolvable host = nil, want error (fail closed)")
	}
}
