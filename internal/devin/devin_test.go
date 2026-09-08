package devin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVarintRoundtrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 32, 1<<63 - 1} {
		enc := encodeVarint(v)
		got, _, err := decodeVarint(enc, 0)
		if err != nil || got != v {
			t.Fatalf("varint %d: got %d err %v", v, got, err)
		}
	}
}

func TestFieldRoundtrip(t *testing.T) {
	raw := concat(
		stringField(1, "hello"),
		varintField(4, 0),
		message(6, concat(stringField(1, "call_1"), stringField(2, "get_w"), stringField(3, "{}"))),
	)
	fields, err := parseFields(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("fields = %d", len(fields))
	}
	if string(fields[0].value) != "hello" || fields[1].vint != 0 {
		t.Fatalf("values: %+v", fields)
	}
}

func TestNormalizeSessionToken(t *testing.T) {
	if got := NormalizeSessionToken("abc"); got != "devin-session-token$abc" {
		t.Fatalf("prefix: %q", got)
	}
	if got := NormalizeSessionToken("devin-session-token$abc"); got != "devin-session-token$abc" {
		t.Fatalf("idempotent: %q", got)
	}
}

func TestTokenExpiryJWT(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	exp := float64(time.Now().Unix() + 3600)
	payload, _ := json.Marshal(map[string]any{"exp": exp})
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	got := TokenExpiry(token)
	want := int64(exp)*1000 - 5*60*1000
	if got != want {
		t.Fatalf("expiry = %d want %d", got, want)
	}
}

func TestTokenExpiryOpaque(t *testing.T) {
	before := time.Now().UnixMilli()
	got := TokenExpiry("opaque-session-token")
	if got < before+364*24*60*60*1000 {
		t.Fatalf("opaque fallback too short: %d", got)
	}
}

func TestBuildChatRequestShape(t *testing.T) {
	in := ChatInput{
		Model: "swe-1-7", System: "Be brief",
		Messages: []WireMessage{
			{Role: WireRoleChat, Text: "hi"},
			{Role: WireRoleChat, ToolCalls: []WireToolCall{{ID: "call_1", Name: "get_w", ArgumentsJSON: `{"city":"Paris"}`}}},
			{Role: WireRoleTool, Text: "sunny", ToolCallID: "call_1"},
		},
		Tools:       []ToolDef{{Name: "get_w", Description: "weather", Parameters: `{"type":"object"}`}},
		Temperature: -1,
		SessionID:   "cascade-1",
	}
	raw := BuildChatRequest(in, "sess", "jwt")
	fields, err := parseFields(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byNum := map[uint64]int{}
	for _, f := range fields {
		byNum[f.number]++
	}
	for _, want := range []uint64{1, 2, 3, 7, 8, 10, 11, 16, 17, 20, 21, 22} {
		if byNum[want] == 0 {
			t.Errorf("missing top-level field %d", want)
		}
	}
	// Three prompts expected.
	if byNum[3] != 3 {
		t.Errorf("prompts = %d want 3", byNum[3])
	}
	if !strings.Contains(string(raw), "swe-1-7") || !strings.Contains(string(raw), "cascade-1") {
		t.Error("model/cascade missing from request")
	}
}

func TestFrameRoundtrip(t *testing.T) {
	payload := []byte("test-payload-bytes")
	framed := FrameConnect(payload)
	frames, rest, err := ParseFrames(framed)
	if err != nil || len(rest) != 0 || len(frames) != 1 {
		t.Fatalf("parse: %v %d %d", err, len(frames), len(rest))
	}
	if frames[0].Trailer || !bytes.Equal(frames[0].Payload, payload) {
		t.Fatalf("roundtrip mismatch: %+v", frames[0])
	}
	// Partial frame carries over.
	half := framed[:len(framed)-3]
	frames, rest, err = ParseFrames(half)
	if err != nil || len(frames) != 0 || len(rest) != len(half) {
		t.Fatalf("partial: %v %d %d", err, len(frames), len(rest))
	}
}

func TestDecodeChatResponse(t *testing.T) {
	payload := concat(
		stringField(1, "msg-1"),
		stringField(3, "Hello"),
		stringField(9, "thinking text"),
		stringField(10, "sig-1"),
		message(6, concat(stringField(1, "call_1"), stringField(2, "get_w"), stringField(3, `{"a":1}`))),
		message(7, concat(varintField(2, 10), varintField(3, 5), varintField(4, 1), varintField(5, 2))),
		concat([]byte{byte(5*8 + 0)}, encodeVarint(1)),
	)
	deltas, err := DecodeChatResponse(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	byType := map[string]Delta{}
	var tools []Delta
	for _, d := range deltas {
		if d.Type == "tool" {
			tools = append(tools, d)
			continue
		}
		byType[d.Type] = d
	}
	if byType["message"].ID != "msg-1" || byType["text"].Text != "Hello" {
		t.Fatalf("message/text: %+v", deltas)
	}
	if th := byType["thinking"]; th.Text != "thinking text" || th.Signature != "sig-1" {
		t.Fatalf("thinking: %+v", th)
	}
	if len(tools) != 1 || tools[0].ID != "call_1" || tools[0].Name != "get_w" || tools[0].ArgsJSON != `{"a":1}` {
		t.Fatalf("tool: %+v", tools)
	}
	if u := byType["usage"]; u.Input != 10 || u.Output != 5 || u.CacheWr != 1 || u.CacheRead != 2 {
		t.Fatalf("usage: %+v", u)
	}
	if byType["stop"].Stop != 1 {
		t.Fatalf("stop: %+v", byType["stop"])
	}
}

func TestTrailerError(t *testing.T) {
	if got := TrailerError([]byte(`{"error":{"message":"boom"}}`)); got != "boom" {
		t.Fatalf("trailer: %q", got)
	}
	if got := TrailerError([]byte(`{}`)); got != "" {
		t.Fatalf("empty trailer: %q", got)
	}
}

func TestDecodeModels(t *testing.T) {
	cfg := func(id, name string, disabled uint64, ctx uint64) []byte {
		return concat(
			stringField(1, name),
			varintField(4, disabled),
			varintField(18, ctx),
			stringField(22, id),
		)
	}
	payload := concat(
		message(1, cfg("swe-1-7", "SWE-1.7 Thinking", 0, 200000)),
		message(1, cfg("swe-1-6", "SWE-1.6", 0, 0)),
		message(1, cfg("old-model", "Old", 1, 1000)),
	)
	models, err := DecodeModels(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "swe-1-6" || models[1].ID != "swe-1-7" {
		t.Fatalf("sorted ids: %+v", models)
	}
	if !models[1].Reasoning || models[1].ContextWindow != 200000 || models[1].MaxTokens != 64000 {
		t.Fatalf("swe-1-7: %+v", models[1])
	}
	if models[0].ContextWindow != 200000 {
		t.Fatalf("default ctx: %+v", models[0])
	}
}

func TestFromOpenAI(t *testing.T) {
	body := `{"model":"devin/swe-1-7","temperature":0.1,"max_tokens":100,"messages":[
		{"role":"system","content":"Be brief"},
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_w","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}],
		"tools":[{"type":"function","function":{"name":"get_w","description":"weather","parameters":{"type":"object"}}}]}`
	in, err := FromOpenAI([]byte(body), "devin/swe-1-7")
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if in.Model != "swe-1-7" || in.System != "Be brief" || in.Temperature != 0.1 || in.MaxTokens != 100 {
		t.Fatalf("header: %+v", in)
	}
	if len(in.Messages) != 3 {
		t.Fatalf("messages = %+v", in.Messages)
	}
	if in.Messages[1].ToolCalls[0].Name != "get_w" || in.Messages[2].Role != WireRoleTool {
		t.Fatalf("tool mapping: %+v", in.Messages)
	}
	if len(in.Tools) != 1 || in.Tools[0].Parameters != `{"type":"object"}` {
		t.Fatalf("tools: %+v", in.Tools)
	}
	// Unset temperature stays negative (reference default applies at build).
	in2, _ := FromOpenAI([]byte(`{"model":"swe-1-7","messages":[]}`), "swe-1-7")
	if in2.Temperature >= 0 {
		t.Fatalf("unset temp: %v", in2.Temperature)
	}
}
