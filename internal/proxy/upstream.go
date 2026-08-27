package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// TimeoutsConfig centralizes every deadline governing an upstream exchange.
// All values are safe defaults; cmd/gateway wires overrides from env/config.
type TimeoutsConfig struct {
	// Dial+TLS+response-header budget per upstream attempt. This replaces the
	// old blanket http.Client{Timeout:120s}, which ALSO killed the response
	// BODY read and thereby decapitated every stream longer than 2 minutes.
	UpstreamHeader time.Duration
	// Overall request budget including retries. Zero disables (streams are
	// bounded instead by StreamIdle).
	RequestTotal time.Duration
	// Maximum gap between stream chunks; resets on every byte received.
	StreamIdle time.Duration
	// Deadline allowed until the FIRST byte reaches the client (header
	// response grace for non-streaming semantics on zero-WriteTimeout servers).
	WriteHeaderGrace time.Duration
}

func DefaultTimeouts() TimeoutsConfig {
	return TimeoutsConfig{
		UpstreamHeader:   120 * time.Second,
		RequestTotal:     0,
		StreamIdle:       300 * time.Second,
		WriteHeaderGrace: 60 * time.Second,
	}
}

// Cloud-metadata endpoints reachable from inside typical VPCs. Outbound dials
// and redirects resolve hostnames, so the check runs against resolved IPs at
// dial time (defeating DNS-rebinding of metadata literals).
var (
	metadataIPv4s = []string{"169.254.169.254", "100.100.100.200"}
	metadataCIDRs = mustParseCIDRs("169.254.169.254/32", "100.100.100.200/32")
)

func mustParseCIDRs(specs ...string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range specs {
		_, n, err := net.ParseCIDR(s)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

func isMetadataIPv6(ip net.IP) bool {
	// AWS IMDS IPv6 endpoint fd00:ec2::254.
	return ip.Equal(net.ParseIP("fd00:ec2::254"))
}

func isMetadataIP(ip net.IP) bool {
	for _, cidr := range metadataCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	if ip.To4() == nil && isMetadataIPv6(ip) {
		return true
	}
	return false
}

// resolveAndCheck pins DNS resolution and rejects cloud-metadata targets.
func resolveAndCheck(ctx context.Context, host string, block bool) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if !block {
		return makeIPs(ips), nil
	}
	for _, ip := range ips {
		if isMetadataIP(ip.IP) {
			return nil, errMetadataBlocked(host)
		}
	}
	return makeIPs(ips), nil
}

func makeIPs(in []net.IPAddr) []net.IP {
	out := make([]net.IP, 0, len(in))
	for _, a := range in {
		out = append(out, a.IP)
	}
	return out
}

type metadataError string

func (m metadataError) Error() string { return string(m) }

const metadataBlockedMsg = "upstream host resolves to a cloud metadata endpoint (blocked)"

func errMetadataBlocked(host string) error { return metadataError(metadataBlockedMsg + ": " + host) }

// NewGatewayTransport builds the shared upstream transport:
//   - explicit dial/TLS/response-header timeouts (no more blanket client
//     timeout killing long-running streams mid-body);
//   - dial-time cloud-metadata rejection (SSRF hardening; extensible later to
//     full private-range denylists via configuration).
func NewGatewayTransport(responseHeaderTimeout time.Duration, blockMetadata bool) *http.Transport {
	base := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	t := &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
			if blockMetadata && isMetadataIP(ip) {
				return nil, errMetadataBlocked(host)
			}
		} else if blockMetadata {
			ctxDial, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err := resolveAndCheck(ctxDial, host, true)
			cancel()
			if err != nil {
				var me metadataError
				if errors.As(err, &me) {
					return nil, err
				}
				// Resolution failure falls through to dialer's own handling.
			}
		}
		return base.DialContext(ctx, network, addr)
	}
	return t
}

// GatewayHTTPClient pairs the hardened transport with strict redirect policy:
// cross-host redirects strip authorization credentials (Go strips
// Authorization/Cookie natively but NOT custom headers like x-api-key, which
// carries decrypted upstream provider keys here).
func GatewayHTTPClient(transport *http.Transport) *http.Client {
	block := true
	return &http.Client{
		Transport: transport,
		Timeout:   0, // handled via per-request contexts and TimeoutsConfig
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("stopped after 3 redirects")
			}
			if block && req.URL != nil && req.URL.Hostname() != "" {
				ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
				_, err := resolveAndCheck(ctx, req.URL.Hostname(), true)
				cancel()
				if err != nil {
					var me metadataError
					if errors.As(err, &me) {
						return err
					}
				}
			}
			if len(via) > 0 {
				prev := via[len(via)-1]
				if prev.URL.Host != req.URL.Host {
					req.Header.Del("Authorization")
					req.Header.Del("x-api-key")
					req.Header.Del("Cookie")
					req.Header.Del("Cookie2")
				}
			}
			return nil
		},
	}
}

var (
	ssnLike         = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	bearerLike      = regexp.MustCompile(`(?i)(bearer|authorization["':= ]+)[A-Za-z0-9._\-]+`)
	apiKeyJSONField = regexp.MustCompile(`("(?:api[_-]?key|x-api-key)"\s*:\s*")[^"]+(")`)
)

// ScrubSecrets removes obvious credential material from captured payload text
// before persisting it anywhere (request/response body debug logging).
func ScrubSecrets(s string) string {
	s = ssnLike.ReplaceAllString(s, "sk-[REDACTED]")
	s = bearerLike.ReplaceAllString(s, "${1}[REDACTED]")
	s = apiKeyJSONField.ReplaceAllString(s, `${1}[REDACTED]${2}`)
	return s
}

// sseEvent holds one completed server-sent-event from a framing-aware parser.
type sseEvent struct {
	name string // event: name (may be empty)
	data []byte // may span multiple data: lines joined by \n
}

// parseSSEEvents extracts complete SSE events from a fully-framed byte slice.
// It tolerates CRLF and comment lines. Partial tails belong to the caller's
// carry-over buffer — feed them back appended ahead of the next read.
func parseSSEEvents(frame []byte) []sseEvent {
	var events []sseEvent
	var cur sseEvent
	hasCur := false
	for len(frame) > 0 {
		nl := bytes.IndexByte(frame, '\n')
		var line []byte
		if nl < 0 {
			line, frame = frame, nil
		} else {
			line, frame = frame[:nl], frame[nl+1:]
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			// Blank line terminates the current event.
			if hasCur {
				events = append(events, cur)
				cur, hasCur = sseEvent{}, false
			}
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte(":")):
			// comment
		case bytes.HasPrefix(line, []byte("event:")):
			cur.name = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			hasCur = true
		case bytes.HasPrefix(line, []byte("data:")):
			d := bytes.TrimPrefix(line, []byte("data:"))
			d = bytes.TrimPrefix(d, []byte(" "))
			if len(cur.data) > 0 {
				cur.data = append(cur.data, '\n')
			}
			cur.data = append(cur.data, d...)
			hasCur = true
		}
	}
	return events
}

// StreamTermination writes a protocol-appropriate terminal sequence so clients
// never hang waiting for frames that will never arrive after an upstream died
// mid-stream. OpenAI-family consumers await `[DONE]`; Anthropic consumers need
// an `error` event (message_stop only follows successful completion).
func writeStreamTerminator(w http.ResponseWriter, flusher http.Flusher, anthropic bool, reason string) {
	var frame string
	if anthropic {
		frame = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"" +
			jsonEscape(reason) + "\"}}\n\n"
	} else {
		frame = "data: {\"error\":{\"message\":\"" + jsonEscape(reason) +
			"\",\"type\":\"server_error\"}}\n\ndata: [DONE]\n\n"
	}
	_, _ = w.Write([]byte(frame))
	if flusher != nil {
		flusher.Flush()
	}
}

func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	// json.Marshal returns the quoted form; strip the surrounding quotes.
	return string(b[1 : len(b)-1])
}
