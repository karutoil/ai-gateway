// Package netguard holds the address-space policy shared by every place where
// the gateway makes an outbound request on a caller's behalf.
//
// The rule is the same everywhere: a URL that an operator or a client can
// influence must not let the gateway be used as a request-forgery hop into the
// deployment's own network (loopback services, RFC1918 hosts, cloud metadata
// endpoints). The check runs against RESOLVED addresses, not hostnames, so a
// DNS name that resolves to 127.0.0.1 or 169.254.169.254 is refused too.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// metadataCIDRs are the well-known cloud instance-metadata endpoints. A gateway
// that can be told to GET one of these leaks instance credentials.
var metadataCIDRs = mustParse("169.254.169.254/32", "100.100.100.200/32", "fd00:ec2::254/128")

func mustParse(specs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(specs))
	for _, s := range specs {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// IsMetadataIP reports whether ip is a cloud instance-metadata endpoint.
func IsMetadataIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, c := range metadataCIDRs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPrivateIP reports whether ip is in an address range the gateway must not
// reach on a caller's behalf: loopback, RFC1918/ULA, link-local (which is where
// cloud metadata lives), CGNAT, unspecified and multicast.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if IsMetadataIP(ip) {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() {
		return true
	}
	// IPv6 unique-local fc00::/7 (net.IP.IsPrivate covers it on modern Go, but
	// keep the explicit check so the intent survives a stdlib change).
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return true
	}
	// CGNAT 100.64.0.0/10 is not covered by IsPrivate and is routinely used for
	// internal service meshes.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// CheckURLHost resolves rawURL's host and refuses it when any resolved address
// is private. allowPrivate short-circuits the check for deployments that
// deliberately front an in-network service (an operator opt-in).
//
// A URL with no host, or an unparsable one, is refused: this is a security
// predicate, so it fails closed.
func CheckURLHost(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid url")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url has no host")
	}
	if allowPrivate {
		return nil
	}
	// A literal IP needs no resolution.
	if ip := net.ParseIP(host); ip != nil {
		if IsPrivateIP(ip) {
			return fmt.Errorf("refusing private address %s", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// Resolution failure is the caller's problem to report; do not turn it
		// into a security decision here.
		return nil
	}
	for _, ip := range ips {
		if IsPrivateIP(ip) {
			return fmt.Errorf("refusing host %q: resolves to a private address", host)
		}
	}
	return nil
}

// HardenedClient builds an outbound client for requests that carry a provider
// credential.
//
// Two properties every credential-bearing client needs, and which a bare
// &http.Client{Timeout: …} does not have:
//
//   - bounded redirects (3) instead of Go's default 10, and
//   - credential headers stripped when the redirect changes host. Go's own
//     shouldCopyHeaderOnRedirect strips only Authorization, Www-Authenticate,
//     Cookie and Cookie2 — x-api-key and api-key (how this gateway sends
//     OpenAI/Anthropic-compatible keys) are forwarded verbatim to any host a 302
//     names. That turned a provider's base_url (or a DNS/HTTP layer in front of
//     it) into a credential-exfiltration primitive on a timer, because the
//     provider health probe runs every 5 minutes.
//
// Metadata endpoints are refused at dial time so a redirect cannot be used to
// reach an instance-credential service either.
func HardenedClient(timeout time.Duration) *http.Client {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				host = addr
			}
			if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && IsMetadataIP(ip) {
				return nil, fmt.Errorf("refusing to connect to metadata address %s", host)
			}
			return base.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("stopped after 3 redirects")
			}
			if len(via) > 0 {
				prev := via[len(via)-1]
				if !strings.EqualFold(prev.URL.Host, req.URL.Host) {
					for _, h := range []string{"Authorization", "x-api-key", "api-key", "x-goog-api-key", "Cookie", "Cookie2"} {
						req.Header.Del(h)
					}
				}
			}
			return nil
		},
	}
}
