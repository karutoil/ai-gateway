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
	"unicode/utf8"
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
	// Client-facing time-to-first-byte budget. The gateway stays silent
	// until upstream headers arrive; behind Cloudflare (~100s edge timeout)
	// that silence becomes a synthesized 524. Zero disables (see
	// DefaultTimeouts for the enabled default).
	TTFB time.Duration
	// ImageRequestTotal bounds one /v1/images/* request end to end. The image
	// endpoints are billable creates that must NEVER be retried (P0-2), so
	// unlike chat they cannot lean on retries to ride out a slow attempt:
	// without an overall budget a wedged upstream holds the connection (and
	// the shutdown grace) for as long as the edge tolerates. It sits above
	// the image first-byte budget so a legitimately slow generation still
	// completes. Zero = DefaultImageRequestTotal.
	//
	// Applies only to images.* endpoints; chat keeps RequestTotal semantics
	// exactly as before.
	ImageRequestTotal time.Duration
	// DecisionRequestTotal bounds one /v1/systemone request end to end. It had
	// no budget at all and inherited the unbounded default (P2-14).
	// Zero = DefaultDecisionRequestTotal.
	DecisionRequestTotal time.Duration
}

// Per-endpoint overall request budgets (P2-14). The decision and image
// endpoints are not chat: their latency profile is different, they are not
// retried, and leaving them unbounded means a wedged provider holds the client
// connection — and the 90s shutdown grace — indefinitely.
const (
	// DefaultImageRequestTotal sits 60s above the image first-byte budget
	// (DefaultImageTTFB = 300s in images.go) so a generation that is slow but
	// progressing still finishes, while a pathological upstream cannot outlive
	// the TTFB window by much.
	DefaultImageRequestTotal = 360 * time.Second
	// DefaultDecisionRequestTotal is generous for a typed evaluation (which
	// returns a small JSON verdict, not a token stream) but bounded.
	DefaultDecisionRequestTotal = 120 * time.Second
)

func DefaultTimeouts() TimeoutsConfig {
	return TimeoutsConfig{
		UpstreamHeader:   120 * time.Second,
		RequestTotal:     0,
		StreamIdle:       300 * time.Second,
		WriteHeaderGrace: 60 * time.Second,
		// Deliberately inside Cloudflare's ~100s first-byte window so the
		// gateway regains control (honest 504 / SSE keepalive) before the
		// edge synthesizes a 524 the gateway never sees.
		TTFB: 85 * time.Second,
		// Per-endpoint overall budgets (P2-14). RequestTotal stays 0 (chat is
		// unchanged, bounded instead by TTFB/StreamIdle).
		ImageRequestTotal:    DefaultImageRequestTotal,
		DecisionRequestTotal: DefaultDecisionRequestTotal,
	}
}

// defaultRequestTotalForEndpoint returns the overall request budget a given
// endpoint gets when the caller did not override it. Chat and the text
// endpoints return zero — their behaviour must not change (they are bounded by
// TTFB and StreamIdle, and operators opt into Timeouts.RequestTotal).
func (t TimeoutsConfig) defaultRequestTotalForEndpoint(endpoint string) time.Duration {
	switch endpoint {
	case "images.generations", "images.edits":
		if t.ImageRequestTotal > 0 {
			return t.ImageRequestTotal
		}
		return DefaultImageRequestTotal
	case "systemone":
		if t.DecisionRequestTotal > 0 {
			return t.DecisionRequestTotal
		}
		return DefaultDecisionRequestTotal
	}
	return 0
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
	return &http.Client{
		Transport:     transport,
		Timeout:       0, // handled via per-request contexts and TimeoutsConfig
		CheckRedirect: RedirectPolicy(),
	}
}

// RedirectPolicy is the shared outbound redirect policy: at most 3 hops, never
// onto a cloud-metadata address, and credentials stripped on a host change.
//
// It is exported so EVERY client that attaches a provider credential shares it.
// The provider health probe and the discovery fetch each built a bare
// &http.Client{Timeout:…}, i.e. Go's default policy: 10 hops, stripping only
// Authorization/Cookie — so the same base_url that is safe on /v1/* leaked the
// decrypted x-api-key/api-key header to any host a 302 pointed at, on a timer
// (health runs every 5 minutes).
func RedirectPolicy() func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("stopped after 3 redirects")
		}
		if req.URL != nil && req.URL.Hostname() != "" {
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
				req.Header.Del("api-key")
				req.Header.Del("Cookie")
				req.Header.Del("Cookie2")
			}
		}
		return nil
	}
}

var (
	ssnLike         = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	bearerLike      = regexp.MustCompile(`(?i)(bearer\s+|authorization["':= ]+)[A-Za-z0-9._\-]+`)
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

// isGeneratedMediaEndpoint reports whether an internal endpoint name returns
// generated binary media (image bytes, video job handles) rather than text.
func isGeneratedMediaEndpoint(endpoint string) bool {
	switch endpoint {
	case "images.generations", "images.edits", "videos", "videos.poll", "videos.list", "videos.content":
		return true
	}
	return false
}

// mediaBodySummary renders a bounded, redacted description of a generated-media
// response body for request_logs.
//
// Why not just truncate: an image response is dominated by a base64 payload in
// `b64_json` (or a provider URL in `url`), so the first N bytes are almost
// entirely opaque base64. Storing that prefix cost ~8 kB of useless data per
// image request in every row and made exports unreadable; it also risks parking
// a chunk of a customer's generated asset in the OLTP database forever. The
// useful facts are the byte count, the response shape and the provider's
// revised_prompt — all of which this keeps.
func mediaBodySummary(body []byte, limit int) string {
	var probe struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Status     string `json:"status"`
		ID         string `json:"id"`
		Object     string `json:"object"`
		Error      any    `json:"error"`
		Message    string `json:"message"`
		Progress   *int   `json:"progress"`
		Seconds    string `json:"seconds"`
		Size       string `json:"size"`
		Created    int64  `json:"created"`
		Completion string `json:"completion"`
	}
	summary := map[string]any{"bytes": len(body), "body": "generated-media (not logged)"}
	if json.Unmarshal(body, &probe) == nil {
		if probe.Object != "" {
			summary["object"] = probe.Object
		}
		if probe.ID != "" {
			summary["id"] = probe.ID
		}
		if probe.Status != "" {
			summary["status"] = probe.Status
		}
		if probe.Progress != nil {
			summary["progress"] = *probe.Progress
		}
		if probe.Seconds != "" {
			summary["seconds"] = probe.Seconds
		}
		if probe.Size != "" {
			summary["size"] = probe.Size
		}
		if probe.Error != nil {
			summary["error"] = probe.Error
		}
		if probe.Message != "" {
			summary["message"] = probe.Message
		}
		if len(probe.Data) > 0 {
			kind := "url"
			var tips []string
			for _, d := range probe.Data {
				if d.B64JSON != "" {
					kind = "b64_json"
				}
				if d.RevisedPrompt != "" && len(tips) < 4 {
					tips = append(tips, d.RevisedPrompt)
				}
			}
			summary["images"] = len(probe.Data)
			summary["image_encoding"] = kind
			if len(tips) > 0 {
				summary["revised_prompt"] = strings.Join(tips, " | ")
			}
		}
	}
	out, err := json.Marshal(summary)
	if err != nil {
		return "generated-media (not logged)"
	}
	if limit > 0 && len(out) > limit {
		return ScrubSecrets(string(out[:limit]))
	}
	return ScrubSecrets(string(out))
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

// truncateUTF8 caps s to max bytes without splitting a UTF-8 rune: a cut
// landing inside a multi-byte sequence backs off to the previous rune
// boundary. Byte-blind caps (s[:n]) can produce invalid UTF-8, which breaks
// JSON strings relayed to clients and — for attribution values — fails the
// whole request_logs INSERT on Postgres (invalid byte sequence for a text
// column), silently dropping the row.
func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	// Walk whole runes while the byte budget allows. A byte-blind cut can
	// land either on a continuation byte OR on a lead byte whose sequence
	// continues past the cap — both split a rune.
	end := 0
	for _, r := range s {
		size := utf8.RuneLen(r)
		if size < 0 || end+size > max {
			break
		}
		end += size
	}
	return s[:end]
}
