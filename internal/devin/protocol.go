package devin

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

// Client identity constants, matching the reference extension wire fingerprint.
const (
	ideName           = "windsurf"
	ideVersion        = "3.2.23"
	extensionVersion  = "1.48.2"
	maxFramePayload   = 16 * 1024 * 1024
	chatPath          = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	userJWTPath       = "/exa.auth_pb.AuthService/GetUserJwt"
	discoveryPath     = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"
	discoveryTimeoutS = 5
)

// ---- protobuf primitives (hand-rolled codec, no .proto compiler needed) ----

type field struct {
	number uint64
	wire   uint64 // 0=varint, 1=64bit, 2=length-delimited, 5=32bit
	value  []byte // wire 2 payload
	vint   uint64 // wire 0 value
}

func encodeVarint(v uint64) []byte {
	var out []byte
	for v > 127 {
		out = append(out, byte(v&127)|128)
		v >>= 7
	}
	return append(out, byte(v))
}

func decodeVarint(buf []byte, off int) (uint64, int, error) {
	var v, shift uint64
	for off < len(buf) {
		b := buf[off]
		off++
		v |= uint64(b&127) << shift
		if b&128 == 0 {
			return v, off, nil
		}
		shift += 7
		if shift > 70 {
			return 0, 0, fmt.Errorf("invalid varint")
		}
	}
	return 0, 0, fmt.Errorf("truncated varint")
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func message(fieldNum uint64, value []byte) []byte {
	return concat(encodeVarint(fieldNum<<3|2), encodeVarint(uint64(len(value))), value)
}

func stringField(fieldNum uint64, value string) []byte {
	return message(fieldNum, []byte(value))
}

func varintField(fieldNum, value uint64) []byte {
	return concat(encodeVarint(fieldNum<<3), encodeVarint(value))
}

func doubleField(fieldNum uint64, value float64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(value))
	return append([]byte{byte(fieldNum*8 + 1)}, b[:]...)
}

func parseFields(buf []byte) ([]field, error) {
	var out []field
	off := 0
	for off < len(buf) {
		key, noff, err := decodeVarint(buf, off)
		if err != nil {
			return nil, err
		}
		off = noff
		number, wire := key>>3, key&7
		switch wire {
		case 0:
			v, noff, err := decodeVarint(buf, off)
			if err != nil {
				return nil, err
			}
			off = noff
			out = append(out, field{number: number, wire: wire, vint: v})
		case 2:
			ln, noff, err := decodeVarint(buf, off)
			if err != nil {
				return nil, err
			}
			off = noff
			end := off + int(ln)
			if end > len(buf) {
				return nil, fmt.Errorf("invalid protobuf length")
			}
			out = append(out, field{number: number, wire: wire, value: buf[off:end]})
			off = end
		case 1:
			off += 8
		case 5:
			off += 4
		default:
			return nil, fmt.Errorf("unsupported wire type %d", wire)
		}
		if off > len(buf) {
			return nil, fmt.Errorf("truncated protobuf")
		}
	}
	return out, nil
}

func firstString(buf []byte, number uint64) string {
	fields, err := parseFields(buf)
	if err != nil {
		return ""
	}
	for _, f := range fields {
		if f.number == number && f.wire == 2 {
			return string(f.value)
		}
	}
	return ""
}

// ---- metadata + auth ----

func encodeMetadata(sessionToken, userJWT string) []byte {
	parts := [][]byte{
		stringField(1, ideName),
		stringField(2, extensionVersion),
		stringField(3, sessionToken),
		stringField(4, "en"),
		stringField(5, runtime.GOOS),
		stringField(7, ideVersion),
		varintField(9, 1),
		stringField(10, uuid.NewString()),
		stringField(12, ideName),
		stringField(25, uuid.NewString()),
		stringField(26, "Unset"),
		stringField(28, ideName),
	}
	if userJWT != "" {
		parts = append(parts, stringField(21, userJWT))
	}
	return concat(parts...)
}

// BuildUserJWTRequest encodes the GetUserJwt request body for a session token.
func BuildUserJWTRequest(sessionToken string) []byte {
	return message(1, encodeMetadata(NormalizeSessionToken(sessionToken), ""))
}

// GetUserJWT exchanges a session token for a short-lived user JWT against
// the given transport base URL (empty selects the default host).
func GetUserJWT(sessionToken, base string, client *http.Client) (string, error) {
	c := client
	if c == nil {
		c = http.DefaultClient
	}
	if strings.TrimSpace(base) == "" {
		base = Host()
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+userJWTPath, bytes.NewReader(BuildUserJWTRequest(sessionToken)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("Accept", "*/*")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("devin auth unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("devin auth failed: %d %s", resp.StatusCode, truncate(string(raw), 200))
	}
	jwt := firstString(raw, 1)
	if jwt == "" {
		return "", fmt.Errorf("devin auth returned an empty user JWT")
	}
	return jwt, nil
}

// ---- chat request ----

// WireRole mirrors the reference roles: 1 = conversational turn, 4 = tool result.
const (
	WireRoleChat = 1
	WireRoleTool = 4
)

// WireToolCall is one tool invocation inside an assistant turn.
type WireToolCall struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

// WireMessage is one history turn on the wire.
type WireMessage struct {
	Role       uint64
	Text       string
	ToolCallID string
	ToolCalls  []WireToolCall
}

// ToolDef is one callable tool.
type ToolDef struct {
	Name        string
	Description string
	Parameters  string // JSON schema document
}

// ChatInput is the normalized input for a Devin chat request.
type ChatInput struct {
	Model    string
	System   string
	Messages []WireMessage
	Tools    []ToolDef
	// Temperature < 0 means unset (reference default 0.4 applies).
	Temperature float64
	// MaxTokens <= 0 means unset (reference default 64000 applies).
	MaxTokens int
	SessionID string // fresh uuid when empty
}

func encodeToolCall(call WireToolCall) []byte {
	return message(6, concat(
		stringField(1, call.ID),
		stringField(2, call.Name),
		stringField(3, call.ArgumentsJSON),
	))
}

func encodePrompt(item WireMessage, id string) []byte {
	parts := [][]byte{
		stringField(1, id),
		varintField(2, item.Role),
		stringField(3, item.Text),
	}
	if item.ToolCallID != "" {
		parts = append(parts, stringField(7, item.ToolCallID))
	}
	for _, call := range item.ToolCalls {
		parts = append(parts, encodeToolCall(call))
	}
	return concat(parts...)
}

func encodeTool(tool ToolDef) []byte {
	params := tool.Parameters
	if params == "" {
		params = "{}"
	}
	description := tool.Description
	if len(description) > 6998 {
		description = description[:6998]
	}
	return concat(
		stringField(1, tool.Name),
		stringField(2, description),
		stringField(3, params),
	)
}

// BuildChatRequest encodes a ChatInput into a raw (unframed) protobuf body.
func BuildChatRequest(in ChatInput, sessionToken, userJWT string) []byte {
	cascade := in.SessionID
	if cascade == "" {
		cascade = uuid.NewString()
	}
	temp := in.Temperature
	if temp < 0 {
		temp = 0.4
	}
	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	configuration := concat(
		varintField(1, 1),
		varintField(2, uint64(maxTokens)),
		varintField(3, 200),
		doubleField(5, temp),
		doubleField(6, temp),
		varintField(7, 50),
		doubleField(8, 1),
		stringField(9, "<|user|>"),
		stringField(9, "<|bot|>"),
		stringField(9, "<|context_request|>"),
		stringField(9, "<|endoftext|>"),
		stringField(9, "<|end_of_turn|>"),
		doubleField(11, 1),
	)
	parts := [][]byte{
		message(1, encodeMetadata(NormalizeSessionToken(sessionToken), userJWT)),
	}
	if in.System != "" {
		parts = append(parts, stringField(2, in.System))
	}
	for i, item := range in.Messages {
		parts = append(parts, message(3, encodePrompt(item, fmt.Sprintf("%s-%d", cascade, i))))
	}
	parts = append(parts,
		varintField(7, 5),
		message(8, configuration),
	)
	for _, tool := range in.Tools {
		parts = append(parts, message(10, encodeTool(tool)))
	}
	parts = append(parts,
		varintField(11, 1),
		stringField(16, cascade),
		stringField(17, uuid.NewString()),
		varintField(20, 1),
		stringField(21, in.Model),
		stringField(22, uuid.NewString()),
	)
	return concat(parts...)
}

// ---- Connect framing (flag byte + BE length, gzip payloads) ----

// FrameConnect gzip-compresses a payload into one Connect frame.
func FrameConnect(payload []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(payload)
	_ = w.Close()
	compressed := buf.Bytes()
	out := make([]byte, 5+len(compressed))
	out[0] = 1
	binary.BigEndian.PutUint32(out[1:5], uint32(len(compressed)))
	copy(out[5:], compressed)
	return out
}

// Frame is one decoded Connect frame.
type Frame struct {
	Trailer bool
	Payload []byte
}

// ParseFrames decodes all complete frames; the remainder is returned for
// incremental readers to carry over.
func ParseFrames(input []byte) (frames []Frame, rest []byte, err error) {
	off := 0
	for off+5 <= len(input) {
		flags := input[off]
		length := int(binary.BigEndian.Uint32(input[off+1 : off+5]))
		if length > maxFramePayload {
			return nil, nil, fmt.Errorf("devin frame exceeds %d bytes", maxFramePayload)
		}
		if off+5+length > len(input) {
			break
		}
		payload := input[off+5 : off+5+length]
		if flags&1 != 0 {
			r, gerr := gzip.NewReader(bytes.NewReader(payload))
			if gerr != nil {
				return nil, nil, fmt.Errorf("devin gunzip: %w", gerr)
			}
			raw, rerr := io.ReadAll(io.LimitReader(r, maxFramePayload+1))
			_ = r.Close()
			if rerr != nil {
				return nil, nil, rerr
			}
			payload = raw
		}
		frames = append(frames, Frame{Trailer: flags&2 != 0, Payload: payload})
		off += 5 + length
	}
	return frames, input[off:], nil
}

// ---- response deltas ----

// Delta is one normalized chat-stream event.
type Delta struct {
	Type      string // message, text, thinking, tool, usage, stop
	Text      string
	ID        string
	Name      string
	ArgsJSON  string
	Signature string
	Input     int
	Output    int
	CacheRead int
	CacheWr   int
	Stop      int
}

// DecodeChatResponse decodes one message payload into deltas.
func DecodeChatResponse(payload []byte) ([]Delta, error) {
	fields, err := parseFields(payload)
	if err != nil {
		return nil, err
	}
	var out []Delta
	for _, f := range fields {
		switch {
		case f.number == 1 && f.wire == 2:
			out = append(out, Delta{Type: "message", ID: string(f.value)})
		case f.number == 3 && f.wire == 2:
			out = append(out, Delta{Type: "text", Text: string(f.value)})
		case f.number == 5 && f.wire == 0:
			out = append(out, Delta{Type: "stop", Stop: int(f.vint)})
		case f.number == 6 && f.wire == 2:
			out = append(out, decodeToolCall(f.value))
		case f.number == 7 && f.wire == 2:
			out = append(out, decodeUsage(f.value))
		case f.number == 9 && f.wire == 2:
			out = append(out, Delta{Type: "thinking", Text: string(f.value)})
		case f.number == 10 && f.wire == 2:
			if n := len(out); n > 0 && out[n-1].Type == "thinking" {
				out[n-1].Signature = string(f.value)
			}
		}
	}
	return out, nil
}

func decodeToolCall(payload []byte) Delta {
	fields, err := parseFields(payload)
	if err != nil {
		return Delta{Type: "tool", Name: "tool"}
	}
	d := Delta{Type: "tool", Name: "tool", ID: uuid.NewString()}
	for _, f := range fields {
		if f.wire != 2 {
			continue
		}
		switch f.number {
		case 1:
			d.ID = string(f.value)
		case 2:
			d.Name = string(f.value)
		case 3:
			d.ArgsJSON = string(f.value)
		}
	}
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.Name == "" {
		d.Name = "tool"
	}
	return d
}

func decodeUsage(payload []byte) Delta {
	fields, err := parseFields(payload)
	if err != nil {
		return Delta{Type: "usage"}
	}
	d := Delta{Type: "usage"}
	for _, f := range fields {
		if f.wire != 0 {
			continue
		}
		switch f.number {
		case 2:
			d.Input = int(f.vint)
		case 3:
			d.Output = int(f.vint)
		case 4:
			d.CacheWr = int(f.vint)
		case 5:
			d.CacheRead = int(f.vint)
		}
	}
	return d
}

// TrailerError extracts a JSON connect trailer error message, if present.
func TrailerError(payload []byte) string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &v) != nil || v.Error == nil {
		return ""
	}
	return v.Error.Message
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
