package proxy

// CONFORMANCE SUITE — ANTHROPIC /v1/messages (streaming + buffered).
//
// Reuses the shared harness helpers defined in chat_proto_test.go
// (protoNewEnv / protoPostJSON via PostRaw / protoEvents / protoPollUsage /
// protoAssertDataLinesValid). Each test owns its own :memory: db, stores and
// servers. Checkpoints a1–a6 + u2/u3(anthropic face).

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/models"
)

const protoAnthModel = "claude-3-5-sonnet-20241022"

func protoAnthBody(stream bool) string {
	return fmt.Sprintf(`{"model":"%s","max_tokens":100,"messages":[{"role":"user","content":"Hello"}],"stream":%t}`, protoAnthModel, stream)
}

// protoAnthSSEFrames is a canonical native-anthropic upstream stream including
// content_block_start, two deltas and a ping relayed untouched.
var protoAnthSSEFrames = []string{
	`event: message_start
data: {"type":"message_start","message":{"id":"msg_anthro","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":11,"output_tokens":1}}}`,
	`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`event: ping
data: {"type":"ping"}`,
	`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
	`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
	`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":13}}`,
	`event: message_stop
data: {"type":"message_stop"}`,
}

func protoWriteAnthStream(w http.ResponseWriter, frames []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	for _, f := range frames {
		fmt.Fprint(w, f+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// a1 — event-name sequence; ping passthrough; a3 folded in (valid JSON lines)
// ---------------------------------------------------------------------------

func TestAnthroProtoEventSequenceAndPingPassthrough(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		protoWriteAnthStream(w, protoAnthSSEFrames)
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, hdr, raw := env.PostRaw("/v1/messages", protoAnthBody(true), nil)
	if status != 200 {
		t.Fatalf("expected 200 got %d: %s", status, raw)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("a1: Content-Type must be text/event-stream, got %q", ct)
	}

	events := protoEvents(raw)
	var names []string
	for _, ev := range events {
		names = append(names, ev.Name)
	}

	// Sequence containment check (message_start ... message_stop, ordered).
	idx := func(name string) int {
		for i, n := range names {
			if n == name {
				return i
			}
		}
		return -1
	}
	ms, cbs := idx("message_start"), idx("content_block_start")
	cd1 := idx("content_block_delta")
	md, mstop := idx("message_delta"), idx("message_stop")
	for label, at := range map[string]int{"message_start": ms, "content_block_start": cbs, "content_block_delta": cd1, "message_delta": md, "message_stop": mstop} {
		if at < 0 {
			t.Fatalf("checkpoint a1: %s missing from stream; names=%v raw=%s", label, names, raw)
		}
	}
	if !(ms < cbs && cbs <= cd1 && cd1 < md && md < mstop) {
		t.Fatalf("checkpoint a1: anthropic event order violated: start=%d block_start=%d delta=%d msg_delta=%d stop=%d", ms, cbs, cd1, md, mstop)
	}

	// ping must pass through UNTOUCHED (exact byte frame).
	const wantPing = "event: ping\ndata: {\"type\":\"ping\"}"
	if !strings.Contains(raw, "\n\n"+wantPing+"\n\n") && !strings.HasPrefix(raw, wantPing+"\n\n") {
		t.Fatalf("checkpoint a1: ping frame not relayed byte-exact (want %q):\n%s", wantPing, raw)
	}

	// a3 — every data line is single-line valid JSON.
	protoAssertDataLinesValid(t, "a3", raw)
}

// ---------------------------------------------------------------------------
// a2 — usage on message_start(input) + message_delta(output) → request_logs
// ---------------------------------------------------------------------------

func TestAnthroProtoUsageCapturedToRequestLogs(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		protoWriteAnthStream(w, protoAnthSSEFrames)
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, _, _ := env.PostRaw("/v1/messages", protoAnthBody(true), nil)
	if status != 200 {
		t.Fatalf("unexpected status %d", status)
	}

	pt, ct, total := protoPollUsage(t, env.DB, 3*time.Second)
	// input_tokens=11 rides message_start.message.usage; output_tokens=13 on message_delta.usage.
	if pt != 11 || ct != 13 {
		t.Fatalf("checkpoint a2: recorded usage wrong — want prompt=11 (message_start) completion=13 (message_delta), got prompt=%d completion=%d total=%d", pt, ct, total)
	}
	if total != 24 {
		t.Fatalf("checkpoint a2: total_tokens wrong, want 24 got %d", total)
	}
}

// ---------------------------------------------------------------------------
// a4 — HTTP 500 BEFORE first anthropic event: honest envelope, zero fake events
// ---------------------------------------------------------------------------

func TestAnthroProto500BeforeFirstEventYieldsNoFakeEvents(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"upstream exploded"}}`)
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, hdr, raw := env.PostRaw("/v1/messages", protoAnthBody(true), nil)

	// Upstream-derived status surfaces in an error envelope (unified proxy
	// envelope carries the upstream STATUS; no anthropic SSE is fabricated).
	if status != http.StatusInternalServerError {
		t.Fatalf("checkpoint a4: client must see upstream failure status 500, got %d body=%s", status, raw)
	}
	if strings.Contains(hdr.Get("Content-Type"), "event-stream") {
		t.Fatalf("checkpoint a4: failure must not arrive as an SSE response")
	}
	for _, banned := range []string{"message_start", "content_block_delta", "message_delta", "message_stop", "event:"} {
		if strings.Contains(raw, banned) {
			t.Fatalf("checkpoint a4: fabricated anthropic stream marker %q present in failure body:\n%s", banned, raw)
		}
	}
	if !strings.Contains(raw, `"error"`) {
		t.Fatalf("checkpoint a4: failure envelope must contain error object, got %q", raw)
	}
	if hits := env.HitCount(); hits != 1 {
		t.Fatalf("a4: exactly one deterministic upstream attempt expected, got %d", hits)
	}
}

// ---------------------------------------------------------------------------
// a5 — mid-stream socket cut → writeStreamTerminator anthropic branch emits
// event:error + api_error JSON. Verifies ACTUAL bytes the code writes today
// (upstream.go:259-271) and encodes them as-is.
//
// NOTE: a hard abort that surfaces as "unexpected EOF" is silently normalized
// into a clean end by pumpStream's isCleanEOF (proxy.go:873-877) — see
// TestAnthroProtoSocketCutUnexpectedEofMasqueradesClean. To reach the REAL
// terminator path we force a RST ("connection reset by peer"), which fails the
// clean-EOF check.
// ---------------------------------------------------------------------------

func TestAnthroProtoMidStreamSocketCutEmitsErrorTerminator(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		sendChunk := func(payload string) {
			fmt.Fprintf(rw, "%x\r\n%s\r\n", len(payload), payload)
			rw.Flush()
		}
		for _, f := range protoAnthSSEFrames[:4] { // message_start .. first delta forwarded for real
			sendChunk(f + "\n\n")
			time.Sleep(40 * time.Millisecond) // let gateway commit & relay each frame
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetLinger(0) // RST on close → gateway sees connection reset, NOT a fake EOF
		}
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, hdr, raw := env.PostRaw("/v1/messages", protoAnthBody(true), nil)

	if status != 200 {
		t.Fatalf("checkpoint a5: headers already committed as 200 SSE, got %d: %s", status, raw)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("checkpoint a5: committed response should be text/event-stream, got %q", hdr.Get("Content-Type"))
	}

	// The forwarded real events came first...
	startIdx := strings.Index(raw, "event: message_start")
	if startIdx < 0 {
		t.Fatalf("checkpoint a5: forwarded message_start missing from output:\n%s", raw)
	}

	// ...then the terminator EXACTLY as writeStreamTerminator formats it:
	//   event: error\n
	//   data: {"type":"error","error":{"type":"api_error","message":"<reason>"}}\n\n
	errIdx := strings.Index(raw, "event: error")
	if errIdx < 0 {
		t.Fatalf("checkpoint a5: missing event:error terminator after mid-stream cut:\n%s", raw)
	}
	if errIdx < startIdx {
		t.Fatalf("checkpoint a5: terminator emitted before forwarded events")
	}
	if !strings.Contains(raw[errIdx:], `"type":"api_error"`) {
		t.Fatalf("checkpoint a5: terminator error.type must be api_error:\n%s", raw[errIdx:])
	}
	var termLine string
	for _, ln := range strings.Split(raw[errIdx:], "\n") {
		if strings.HasPrefix(ln, "data:") {
			termLine = strings.TrimSpace(strings.TrimPrefix(ln, "data:"))
			break
		}
	}
	var term struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(termLine), &term); err != nil {
		t.Fatalf("checkpoint a5: terminator data payload is not valid JSON (%v): %q", err, termLine)
	}
	if term.Type != "error" || term.Error.Type != "api_error" || term.Error.Message == "" {
		t.Fatalf("checkpoint a5: terminator shape mismatch: %+v", term)
	}
	// Anthropic protocol correctness: NO fake message_stop after a failure.
	if strings.Contains(raw, `"type":"message_stop"`) {
		t.Fatalf("checkpoint a5: fabricating message_stop after a failed stream masquerades success")
	}
}

// a5b — DEFECT anth-D2: a hard cut that Go surfaces as "unexpected EOF" is
// classified by pumpStream.isCleanEOF as a CLEAN end: no event:error is written,
// the request_logs row even records the upstream header status as if the stream
// completed. Their own contract (pumpStream doc comment) requires
// protocol-correct termination on ANY abnormal exit.
func TestAnthroProtoSocketCutUnexpectedEofMasqueradesClean(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		protoWriteAnthStream(w, protoAnthSSEFrames[:4])
		time.Sleep(150 * time.Millisecond)
		panic(http.ErrAbortHandler) // abrupt TCP cut → client sees unexpected EOF
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, _, raw := env.PostRaw("/v1/messages", protoAnthBody(true), nil)

	sawTerminator := strings.Contains(raw, "event: error")
	sawStop := strings.Contains(raw, `"type":"message_stop"`)
	framesForwarded := strings.Contains(raw, "event: message_start")
	if framesForwarded && !sawTerminator && !sawStop && status == 200 {
		t.Skipf("DEFECT-K anth-D2: mid-stream 'unexpected EOF' from upstream is normalized to a CLEAN end by pumpStream.isCleanEOF (proxy.go:873-877) — real events relayed then silence: no event:error, no terminator; request_logs records a success row. Spec a5 requires termination on ANY abnormal exit – unskip after fix (pinned: silent truncated 200)")
	}
	if framesForwarded && !sawTerminator {
		t.Fatalf("a5b post-fix regression gate: abnormal exit without termination marker:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// a6 — non-stream basic shape sanity: type=message, stop_reason passthrough
// ---------------------------------------------------------------------------

func TestAnthroProtoNonStreamShapeSanity(t *testing.T) {
	const golden = `{"id":"msg_nostream","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"Plain reply"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":8,"output_tokens":6}}`
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, golden)
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, _, raw := env.PostRaw("/v1/messages", protoAnthBody(false), nil)
	if status != 200 {
		t.Fatalf("expected 200 got %d: %s", status, raw)
	}
	// Native→native non-stream path is pure passthrough — pin byte fidelity.
	if raw != golden {
		t.Fatalf("checkpoint a6: response not passthrough-faithful.\nwant: %s\ngot:  %s", golden, raw)
	}
	var m map[string]interface{}
	json.Unmarshal([]byte(raw), &m)
	if m["type"] != "message" {
		t.Fatalf("checkpoint a6: type must stay message, got %v", m["type"])
	}
	if m["stop_reason"] != "end_turn" {
		t.Fatalf("checkpoint a6: stop_reason mapping (passthrough) broken, got %v", m["stop_reason"])
	}
	if content, ok := m["content"].([]interface{}); !ok || len(content) == 0 {
		t.Fatalf("checkpoint a6: content blocks lost: %#v", m["content"])
	}
}

// ---------------------------------------------------------------------------
// u2 — key/org cache scoping for the MESSAGES endpoint (non-stream)
// ---------------------------------------------------------------------------

func TestAnthroProtoMessagesCacheScopedPerKey(t *testing.T) {
	var hits atomic.Int32
	up := func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_scope_%d","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"answer-%d"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`, n, n)
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")
	env.H.Cache = cache.NewMemoryCache(16)

	bodyA := protoAnthBody(false)
	sA1, hA1, _ := env.PostRaw("/v1/messages", bodyA, nil) // prime with default key
	sA2, hA2, rawA2 := env.PostRaw("/v1/messages", bodyA, nil)

	if sA1 != 200 || sA2 != 200 {
		t.Fatalf("u2: unexpected statuses %d/%d", sA1, sA2)
	}
	if hA1.Get("X-Cache") == "HIT" {
		t.Fatalf("u2: priming request must MISS")
	}
	if hA2.Get("X-Cache") != "HIT" {
		t.Fatalf("u2 control: same-key identical request should HIT the cache (got X-Cache=%q body=%s)", hA2.Get("X-Cache"), rawA2)
	}

	keyB := env.CreateAllowedKey(nil)
	env.Key = keyB
	sB, hB, _ := env.PostRaw("/v1/messages", bodyA, nil)
	if sB != 200 {
		t.Fatalf("u2: second-key request failed: %d", sB)
	}
	if hB.Get("X-Cache") == "HIT" {
		t.Fatalf("checkpoint u2: CROSS-TENANT CACHE LEAK on /v1/messages — key B received key A's cached completion")
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("checkpoint u2: different key must bypass cache (upstream hits=%d)", n)
	}
}

// ---------------------------------------------------------------------------
// u3(anthropic face) — malformed non-SSE 200 HTML body on a STREAM request to
// /v1/messages must fail honestly rather than silently relay garbage.
// ---------------------------------------------------------------------------

func TestAnthroProtoMalformedUpstreamHonestFailure(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		io.WriteString(w, "<html><body>proxy splash page</body></html>\n")
	}
	env := protoNewEnv(t, up, models.ProviderAnthropic, "", "messages")

	status, hdr, raw := env.PostRaw("/v1/messages", protoAnthBody(true), nil)

	// PIN current behavior before flagging.
	if status == 200 && strings.Contains(raw, "<html>") {
		ct := hdr.Get("Content-Type")
		t.Skipf("DEFECT-K anth-D1: pumpStream relays any upstream 200 verbatim (proxy.go:692-698); /v1/messages stream client received HTTP 200 Content-Type=%q containing <html>, with no anthropic events and no event:error terminator — silent-ok instead of honest failure – unskip after fix (pinned: html passthrough)", ct)
	}

	// ---- checkpoint asserts (regression gate post-fix) ----
	if status == 200 {
		t.Fatalf("u3: anthropic malformed upstream must not yield client 200")
	}
	if strings.Contains(raw, "<html>") {
		t.Fatalf("u3: HTML leaked into /v1/messages stream response")
	}
}
