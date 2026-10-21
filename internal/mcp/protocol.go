package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCall is one model-selected tool invocation, normalized across dialects.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
	Input     map[string]any
}

// CallSummary is the structured execution metadata reported to clients.
type CallSummary struct {
	Server     string `json:"server"`
	Tool       string `json:"tool"`
	Name       string `json:"name"`
	CallID     string `json:"call_id,omitempty"`
	Round      int    `json:"round"`
	IsError    bool   `json:"is_error"`
	DurationMs int64  `json:"duration_ms"`
}

// ParseToolCalls extracts tool calls from a buffered model response. It
// handles both JSON and SSE bodies for all three inbound protocols.
func ParseToolCalls(proto Protocol, contentType string, body []byte) []ToolCall {
	trimmed := strings.TrimSpace(string(body))
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		strings.HasPrefix(trimmed, "data:") || strings.Contains(trimmed, "\ndata:") ||
		strings.HasPrefix(trimmed, "event:")
	if isSSE {
		return parseSSEToolCalls(proto, body)
	}
	return parseJSONToolCalls(proto, body)
}

func parseJSONToolCalls(proto Protocol, body []byte) []ToolCall {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	switch proto {
	case ProtocolAnthropic:
		var content []map[string]json.RawMessage
		if raw, ok := root["content"]; ok {
			_ = json.Unmarshal(raw, &content)
		}
		var calls []ToolCall
		for _, block := range content {
			var typ string
			_ = json.Unmarshal(block["type"], &typ)
			if typ != "tool_use" {
				continue
			}
			c := ToolCall{}
			_ = json.Unmarshal(block["id"], &c.ID)
			_ = json.Unmarshal(block["name"], &c.Name)
			if raw, ok := block["input"]; ok {
				c.Arguments = string(raw)
				_ = json.Unmarshal(raw, &c.Input)
			}
			if c.Input == nil {
				c.Input = map[string]any{}
			}
			calls = append(calls, c)
		}
		return calls
	case ProtocolResponses:
		var output []map[string]json.RawMessage
		if raw, ok := root["output"]; ok {
			_ = json.Unmarshal(raw, &output)
		}
		var calls []ToolCall
		for _, item := range output {
			var typ string
			_ = json.Unmarshal(item["type"], &typ)
			if typ != "function_call" {
				continue
			}
			c := ToolCall{}
			_ = json.Unmarshal(item["call_id"], &c.ID)
			if c.ID == "" {
				_ = json.Unmarshal(item["id"], &c.ID)
			}
			_ = json.Unmarshal(item["name"], &c.Name)
			_ = json.Unmarshal(item["arguments"], &c.Arguments)
			c.Input = decodeArgs(c.Arguments)
			calls = append(calls, c)
		}
		return calls
	default:
		var choices []map[string]json.RawMessage
		if raw, ok := root["choices"]; ok {
			_ = json.Unmarshal(raw, &choices)
		}
		var calls []ToolCall
		for _, ch := range choices {
			var msg map[string]json.RawMessage
			if raw, ok := ch["message"]; ok {
				_ = json.Unmarshal(raw, &msg)
			}
			var tcs []map[string]json.RawMessage
			if raw, ok := msg["tool_calls"]; ok {
				_ = json.Unmarshal(raw, &tcs)
			}
			for _, tc := range tcs {
				c := ToolCall{}
				_ = json.Unmarshal(tc["id"], &c.ID)
				var fn map[string]json.RawMessage
				if raw, ok := tc["function"]; ok {
					_ = json.Unmarshal(raw, &fn)
				}
				_ = json.Unmarshal(fn["name"], &c.Name)
				_ = json.Unmarshal(fn["arguments"], &c.Arguments)
				c.Input = decodeArgs(c.Arguments)
				calls = append(calls, c)
			}
			// Legacy single function_call shape.
			var fc map[string]json.RawMessage
			if raw, ok := msg["function_call"]; ok && len(tcs) == 0 {
				_ = json.Unmarshal(raw, &fc)
				if len(fc) > 0 {
					c := ToolCall{}
					_ = json.Unmarshal(fc["name"], &c.Name)
					_ = json.Unmarshal(fc["arguments"], &c.Arguments)
					c.ID = "call_legacy"
					c.Input = decodeArgs(c.Arguments)
					calls = append(calls, c)
				}
			}
		}
		return calls
	}
}

func decodeArgs(s string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{"_raw": s}
	}
	return out
}

type partialCall struct {
	ToolCall
	index int
	done  bool
}

func parseSSEToolCalls(proto Protocol, body []byte) []ToolCall {
	events := sseData(body)
	switch proto {
	case ProtocolAnthropic:
		return parseAnthropicSSE(events)
	case ProtocolResponses:
		return parseResponsesSSE(events)
	default:
		return parseChatSSE(events)
	}
}

func sseData(body []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		out = append(out, []byte(payload))
	}
	return out
}

func parseChatSSE(events [][]byte) []ToolCall {
	byIndex := map[int]*partialCall{}
	order := []int{}
	for _, ev := range events {
		var root map[string]json.RawMessage
		if json.Unmarshal(ev, &root) != nil {
			continue
		}
		var choices []map[string]json.RawMessage
		if raw, ok := root["choices"]; ok {
			_ = json.Unmarshal(raw, &choices)
		}
		for _, ch := range choices {
			var delta map[string]json.RawMessage
			if raw, ok := ch["delta"]; ok {
				_ = json.Unmarshal(raw, &delta)
			}
			var tcs []map[string]json.RawMessage
			if raw, ok := delta["tool_calls"]; ok {
				_ = json.Unmarshal(raw, &tcs)
			}
			for _, tc := range tcs {
				var idx int
				_ = json.Unmarshal(tc["index"], &idx)
				pc, ok := byIndex[idx]
				if !ok {
					pc = &partialCall{index: idx}
					byIndex[idx] = pc
					order = append(order, idx)
				}
				var id string
				if json.Unmarshal(tc["id"], &id) == nil && id != "" {
					pc.ID = id
				}
				var fn map[string]json.RawMessage
				if raw, ok := tc["function"]; ok {
					_ = json.Unmarshal(raw, &fn)
				}
				var name, args string
				if json.Unmarshal(fn["name"], &name) == nil && name != "" {
					pc.Name = name
				}
				if json.Unmarshal(fn["arguments"], &args) == nil {
					pc.Arguments += args
				}
			}
		}
	}
	var calls []ToolCall
	for _, idx := range order {
		pc := byIndex[idx]
		if pc.Name == "" {
			continue
		}
		if pc.ID == "" {
			pc.ID = fmt.Sprintf("call_mcp_%d", idx)
		}
		pc.Input = decodeArgs(pc.Arguments)
		calls = append(calls, pc.ToolCall)
	}
	return calls
}

func parseResponsesSSE(events [][]byte) []ToolCall {
	seen := map[string]bool{}
	var calls []ToolCall
	add := func(item map[string]json.RawMessage) {
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if typ != "function_call" {
			return
		}
		c := ToolCall{}
		_ = json.Unmarshal(item["call_id"], &c.ID)
		if c.ID == "" {
			_ = json.Unmarshal(item["id"], &c.ID)
		}
		_ = json.Unmarshal(item["name"], &c.Name)
		_ = json.Unmarshal(item["arguments"], &c.Arguments)
		if c.Name == "" || seen[c.ID+c.Name] {
			return
		}
		seen[c.ID+c.Name] = true
		c.Input = decodeArgs(c.Arguments)
		calls = append(calls, c)
	}
	for _, ev := range events {
		var root map[string]json.RawMessage
		if json.Unmarshal(ev, &root) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(root["type"], &typ)
		switch typ {
		case "response.output_item.done":
			var item map[string]json.RawMessage
			if raw, ok := root["item"]; ok {
				_ = json.Unmarshal(raw, &item)
				add(item)
			}
		case "response.completed":
			var resp map[string]json.RawMessage
			if raw, ok := root["response"]; ok {
				_ = json.Unmarshal(raw, &resp)
			}
			var output []map[string]json.RawMessage
			if raw, ok := resp["output"]; ok {
				_ = json.Unmarshal(raw, &output)
			}
			for _, item := range output {
				add(item)
			}
		}
	}
	return calls
}

func parseAnthropicSSE(events [][]byte) []ToolCall {
	byIndex := map[int]*partialCall{}
	order := []int{}
	for _, ev := range events {
		var root map[string]json.RawMessage
		if json.Unmarshal(ev, &root) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(root["type"], &typ)
		switch typ {
		case "content_block_start":
			var idx int
			_ = json.Unmarshal(root["index"], &idx)
			var block map[string]json.RawMessage
			if raw, ok := root["content_block"]; ok {
				_ = json.Unmarshal(raw, &block)
			}
			var btyp string
			_ = json.Unmarshal(block["type"], &btyp)
			if btyp != "tool_use" {
				continue
			}
			pc := &partialCall{index: idx}
			_ = json.Unmarshal(block["id"], &pc.ID)
			_ = json.Unmarshal(block["name"], &pc.Name)
			if raw, ok := block["input"]; ok && string(raw) != "{}" {
				pc.Arguments = string(raw)
			}
			byIndex[idx] = pc
			order = append(order, idx)
		case "content_block_delta":
			var idx int
			_ = json.Unmarshal(root["index"], &idx)
			pc, ok := byIndex[idx]
			if !ok {
				continue
			}
			var delta map[string]json.RawMessage
			if raw, ok := root["delta"]; ok {
				_ = json.Unmarshal(raw, &delta)
			}
			var dtype string
			_ = json.Unmarshal(delta["type"], &dtype)
			if dtype == "input_json_delta" {
				var partial string
				_ = json.Unmarshal(delta["partial_json"], &partial)
				pc.Arguments += partial
			}
		}
	}
	var calls []ToolCall
	for _, idx := range order {
		pc := byIndex[idx]
		if pc.Name == "" {
			continue
		}
		if pc.ID == "" {
			pc.ID = fmt.Sprintf("toolu_mcp_%d", idx)
		}
		pc.Input = decodeArgs(pc.Arguments)
		calls = append(calls, pc.ToolCall)
	}
	return calls
}

// AppendToolResults appends the assistant tool-call turn and the matching tool
// results to the request body, in the inbound protocol's own shape.
func AppendToolResults(proto Protocol, body []byte, calls []ToolCall, results []ToolResult, assistantText string) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	switch proto {
	case ProtocolAnthropic:
		var messages []any
		if raw, ok := root["messages"]; ok {
			_ = json.Unmarshal(raw, &messages)
		}
		assistant := map[string]any{"role": "assistant"}
		content := []any{}
		if assistantText != "" {
			content = append(content, map[string]any{"type": "text", "text": assistantText})
		}
		for _, c := range calls {
			content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Input})
		}
		assistant["content"] = content
		messages = append(messages, assistant)
		var toolResults []any
		for i, r := range results {
			text := r.Text
			if text == "" {
				text = "(empty result)"
			}
			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": callID(calls, i),
				"content":     text,
				"is_error":    r.IsError,
			})
		}
		messages = append(messages, map[string]any{"role": "user", "content": toolResults})
		raw, _ := json.Marshal(messages)
		root["messages"] = raw
	case ProtocolResponses:
		var input []any
		if raw, ok := root["input"]; ok {
			if json.Unmarshal(raw, &input) != nil {
				var s string
				if json.Unmarshal(raw, &s) == nil && s != "" {
					input = []any{map[string]any{"role": "user", "content": s}}
				}
			}
		}
		for i, c := range calls {
			input = append(input, map[string]any{
				"type":      "function_call",
				"call_id":   c.ID,
				"name":      c.Name,
				"arguments": c.Arguments,
			})
			text := results[i].Text
			if text == "" {
				text = "(empty result)"
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": c.ID,
				"output":  text,
			})
		}
		raw, _ := json.Marshal(input)
		root["input"] = raw
	default:
		var messages []any
		if raw, ok := root["messages"]; ok {
			_ = json.Unmarshal(raw, &messages)
		}
		var toolCalls []any
		for _, c := range calls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Arguments,
				},
			})
		}
		assistant := map[string]any{"role": "assistant", "tool_calls": toolCalls}
		if assistantText != "" {
			assistant["content"] = assistantText
		} else {
			assistant["content"] = nil
		}
		messages = append(messages, assistant)
		for i, r := range results {
			text := r.Text
			if text == "" {
				text = "(empty result)"
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": callID(calls, i),
				"content":      text,
			})
		}
		raw, _ := json.Marshal(messages)
		root["messages"] = raw
	}
	return json.Marshal(root)
}

func callID(calls []ToolCall, i int) string {
	if i < len(calls) {
		if calls[i].ID != "" {
			return calls[i].ID
		}
	}
	return fmt.Sprintf("call_mcp_%d", i)
}

// InjectMetadata adds structured execution metadata to a buffered response
// without breaking the protocol shape:
//
//   - JSON bodies get additive top-level mcp_rounds / mcp_tool_calls fields.
//   - SSE bodies get the fields merged into an existing terminal frame, so
//     strict SDK parsers never see an unknown event type.
func InjectMetadata(proto Protocol, contentType string, body []byte, rounds int, calls []CallSummary, loopErr string) []byte {
	trimmed := strings.TrimSpace(string(body))
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, "event:")
	if !isSSE {
		var root map[string]json.RawMessage
		if json.Unmarshal(body, &root) != nil {
			return body
		}
		r, _ := json.Marshal(rounds)
		root["mcp_rounds"] = r
		if len(calls) > 0 {
			c, _ := json.Marshal(calls)
			root["mcp_tool_calls"] = c
		}
		if loopErr != "" {
			e, _ := json.Marshal(loopErr)
			root["mcp_error"] = e
		}
		out, err := json.Marshal(root)
		if err != nil {
			return body
		}
		return out
	}
	meta := map[string]any{"mcp_rounds": rounds}
	if len(calls) > 0 {
		meta["mcp_tool_calls"] = calls
	}
	if loopErr != "" {
		meta["mcp_error"] = loopErr
	}
	return injectSSEMetadata(proto, body, meta)
}

func injectSSEMetadata(proto Protocol, body []byte, meta map[string]any) []byte {
	lines := strings.Split(string(body), "\n")
	// Target frame selection differs per dialect: the last data frame that
	// already carries a JSON object we can extend.
	best := -1
	bestScore := -1
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		score := 0
		switch proto {
		case ProtocolChat:
			if _, ok := obj["choices"]; ok {
				score = 1
				if raw, ok := obj["choices"]; ok && strings.Contains(string(raw), `"finish_reason":"`) {
					score = 2
				}
			}
		case ProtocolResponses:
			var typ string
			_ = json.Unmarshal(obj["type"], &typ)
			if typ == "response.completed" {
				score = 3
			} else if typ != "" {
				score = 1
			}
		case ProtocolAnthropic:
			var typ string
			_ = json.Unmarshal(obj["type"], &typ)
			if typ == "message_delta" {
				score = 3
			} else if typ != "" {
				score = 1
			}
		}
		if score > bestScore {
			bestScore = score
			best = i
		}
	}
	if best < 0 {
		return body
	}
	line := strings.TrimRight(lines[best], "\r")
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return body
	}
	for k, v := range meta {
		obj[k] = v
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	lines[best] = "data: " + string(encoded)
	return []byte(strings.Join(lines, "\n"))
}

// JSONToSSE converts a buffered completion body into a valid SSE stream for
// the given protocol. The gateway buffers every tool round (a stream cannot be
// inspected mid-flight), so a client that asked for `stream: true` would
// otherwise receive a bare JSON document on a text/event-stream connection.
// Emitting the complete answer as a single well-formed event sequence keeps
// every OpenAI/Anthropic SDK parser on its normal path.
func JSONToSSE(proto Protocol, body []byte) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	var sb strings.Builder
	switch proto {
	case ProtocolAnthropic:
		msg := map[string]any{"type": "message_start", "message": json.RawMessage(body)}
		b, _ := json.Marshal(msg)
		sb.WriteString("event: message_start\ndata: " + string(b) + "\n\n")
		// Replay the text blocks as content deltas so token-oriented clients
		// still see the answer text.
		var content []map[string]json.RawMessage
		if raw, ok := root["content"]; ok {
			_ = json.Unmarshal(raw, &content)
		}
		for i, block := range content {
			var typ, text string
			_ = json.Unmarshal(block["type"], &typ)
			if typ != "text" {
				continue
			}
			_ = json.Unmarshal(block["text"], &text)
			start, _ := json.Marshal(map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			sb.WriteString("event: content_block_start\ndata: " + string(start) + "\n\n")
			delta, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
			sb.WriteString("event: content_block_delta\ndata: " + string(delta) + "\n\n")
			stop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": i})
			sb.WriteString("event: content_block_stop\ndata: " + string(stop) + "\n\n")
		}
		var stopReason any = "end_turn"
		if raw, ok := root["stop_reason"]; ok && string(raw) != "null" {
			stopReason = raw
		}
		tail, _ := json.Marshal(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason},
		})
		sb.WriteString("event: message_delta\ndata: " + string(tail) + "\n\n")
		sb.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case ProtocolResponses:
		done, _ := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(body)})
		sb.WriteString("data: " + string(done) + "\n\n")
	default:
		// Chat completions: one delta carrying the assistant message, then a
		// finish frame and the [DONE] sentinel.
		var choices []map[string]json.RawMessage
		if raw, ok := root["choices"]; ok {
			_ = json.Unmarshal(raw, &choices)
		}
		for _, ch := range choices {
			var msg map[string]json.RawMessage
			if raw, ok := ch["message"]; ok {
				_ = json.Unmarshal(raw, &msg)
			}
			var finish string
			_ = json.Unmarshal(ch["finish_reason"], &finish)
			contentText := "null"
			if raw, ok := msg["content"]; ok && string(raw) != "null" {
				contentText = string(raw)
			}
			delta, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"role": "assistant", "content": json.RawMessage(contentText)},
				}},
			})
			sb.WriteString("data: " + string(delta) + "\n\n")
			if finish == "" {
				finish = "stop"
			}
			fin, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
			})
			sb.WriteString("data: " + string(fin) + "\n\n")
		}
		sb.WriteString("data: [DONE]\n\n")
	}
	return []byte(sb.String())
}
