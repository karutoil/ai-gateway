package netguard

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// TestIsPrivateIP pins the address-space policy that every outbound fetch on a
// caller's behalf depends on. Without these cases, making IsPrivateIP return
// false leaves the whole suite green while turning the MCP registry (and any
// other URL-driven fetch) into an SSRF primitive that can read cloud instance
// credentials.
func TestIsPrivateIP(t *testing.T) {
	private := []string{
		"127.0.0.1",       // loopback
		"127.1.2.3",       // loopback, non-canonical
		"::1",             // IPv6 loopback
		"10.0.0.1",        // RFC1918
		"172.16.0.1",      // RFC1918
		"172.31.255.254",  // RFC1918 upper bound
		"192.168.1.1",     // RFC1918
		"169.254.169.254", // AWS/GCP/Azure metadata
		"169.254.1.1",     // link-local
		"100.64.0.1",      // CGNAT / service mesh
		"100.127.255.254", // CGNAT upper bound
		"fd00:ec2::254",   // IPv6 metadata
		"fc00::1",         // IPv6 unique-local
		"fdff:ffff::1",    // IPv6 unique-local upper bound
		"fe80::1",         // IPv6 link-local
		"0.0.0.0",         // unspecified
		"::",              // unspecified v6
		"224.0.0.1",       // multicast
		"ff02::1",         // multicast v6
	}
	for _, s := range private {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("IsPrivateIP(%s) = false, want true", s)
		}
	}

	public := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34", // example.com
		"172.15.255.255",
		"172.32.0.1", // just outside RFC1918
		"100.63.255.255",
		"100.128.0.1", // just outside CGNAT
		"2606:4700:4700::1111",
	}
	for _, s := range public {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("IsPrivateIP(%s) = true, want false", s)
		}
	}

	if !IsPrivateIP(nil) {
		t.Error("IsPrivateIP(nil) must fail closed (true)")
	}
}

func TestIsMetadataIP(t *testing.T) {
	for _, s := range []string{"169.254.169.254", "100.100.100.200", "fd00:ec2::254"} {
		if !IsMetadataIP(net.ParseIP(s)) {
			t.Errorf("IsMetadataIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "10.0.0.1", "127.0.0.1"} {
		if IsMetadataIP(net.ParseIP(s)) {
			t.Errorf("IsMetadataIP(%s) = true, want false", s)
		}
	}
	if IsMetadataIP(nil) {
		t.Error("IsMetadataIP(nil) must be false")
	}
}

func TestCheckURLHostRefusesPrivateAndMetadata(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:8080/mcp",
		"http://localhost:3000/mcp",
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://10.0.0.5/mcp",
		"http://[::1]:9000/mcp",
		"https://192.168.0.10/mcp",
		"http://100.64.0.9/mcp",
	}
	for _, u := range cases {
		if err := CheckURLHost(u, false); err == nil {
			t.Errorf("CheckURLHost(%q, allowPrivate=false) = nil, want an error", u)
		}
	}
}

func TestCheckURLHostAllowsPrivateWhenOptedIn(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080/mcp", "http://10.0.0.5/mcp"} {
		if err := CheckURLHost(u, true); err != nil {
			t.Errorf("CheckURLHost(%q, allowPrivate=true) = %v, want nil", u, err)
		}
	}
}

func TestCheckURLHostAllowsPublic(t *testing.T) {
	if err := CheckURLHost("https://mcp.example.com/mcp", false); err != nil {
		t.Errorf("public URL refused: %v", err)
	}
}

func TestCheckURLHostFailsClosedOnGarbage(t *testing.T) {
	for _, u := range []string{"", "://nope", "http://", "not a url at all"} {
		if err := CheckURLHost(u, true); err == nil {
			t.Errorf("CheckURLHost(%q) = nil, want an error (must fail closed)", u)
		}
	}
}

// TestHardenedClientStripsCredentialsOnHostChange is the regression test for the
// health-probe/discovery credential leak: Go's default redirect policy strips
// only Authorization/Cookie, so x-api-key and api-key — how this gateway sends
// provider keys — were forwarded to any host a 302 named.
func TestHardenedClientStripsCredentialsOnHostChange(t *testing.T) {
	client := HardenedClient(2 * time.Second)

	// Build two requests the way a redirect chain would.
	origin, _ := http.NewRequest(http.MethodGet, "https://provider.example.com/v1/models", nil)
	origin.Header.Set("Authorization", "Bearer secret")
	origin.Header.Set("x-api-key", "sk-ant-secret")
	origin.Header.Set("api-key", "azure-secret")
	origin.Header.Set("x-goog-api-key", "google-secret")
	origin.Header.Set("Cookie", "session=1")

	// Same host: credentials are preserved (providers legitimately redirect
	// within their own API surface).
	same, _ := http.NewRequest(http.MethodGet, "https://provider.example.com/v1/models?page=2", nil)
	same.Header = origin.Header.Clone()
	if err := client.CheckRedirect(same, []*http.Request{origin}); err != nil {
		t.Fatalf("same-host redirect refused: %v", err)
	}
	if same.Header.Get("x-api-key") == "" {
		t.Error("same-host redirect must keep x-api-key")
	}

	// Cross host: every credential header must be gone.
	cross, _ := http.NewRequest(http.MethodGet, "https://attacker.example.net/collect", nil)
	cross.Header = origin.Header.Clone()
	if err := client.CheckRedirect(cross, []*http.Request{origin}); err != nil {
		t.Fatalf("cross-host redirect refused: %v", err)
	}
	for _, h := range []string{"Authorization", "x-api-key", "api-key", "x-goog-api-key", "Cookie"} {
		if v := cross.Header.Get(h); v != "" {
			t.Errorf("cross-host redirect leaked %s = %q", h, v)
		}
	}
}

func TestHardenedClientCapsRedirects(t *testing.T) {
	client := HardenedClient(2 * time.Second)
	via := make([]*http.Request, 3)
	for i := range via {
		via[i], _ = http.NewRequest(http.MethodGet, "https://a.example.com/x", nil)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://a.example.com/y", nil)
	if err := client.CheckRedirect(req, via); err == nil {
		t.Error("expected redirect cap to refuse the 4th hop")
	}
}
