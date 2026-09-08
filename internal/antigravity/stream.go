package antigravity

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

// ParsedChunk is one normalized Antigravity SSE data payload.
type ParsedChunk struct {
	Texts     []string
	Thoughts  []string
	ToolCalls []ParsedToolCall
	Usage     Usage
	Finish    string
	HasData   bool
}

// ParsedToolCall is one functionCall part.
type ParsedToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// Usage mirrors usageMetadata.
type Usage struct {
	Input     int
	Output    int
	CacheRead int
	Reasoning int
	Total     int
	HasUsage  bool
}

// ParseChunk parses one SSE data: line body (JSON).
func ParseChunk(data string) ParsedChunk {
	var out ParsedChunk
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return out
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return out
	}
	if em, ok := chunk["error"].(map[string]any); ok {
		if msg, _ := em["message"].(string); msg != "" {
			out.Finish = "error:" + msg
			out.HasData = true
		}
		return out
	}
	resp := chunk
	if r, ok := chunk["response"].(map[string]any); ok && r != nil {
		resp = r
	}
	cands, _ := resp["candidates"].([]any)
	if len(cands) == 0 {
		// Still check usage-only frames.
		if u := parseUsage(resp["usageMetadata"]); u.HasUsage {
			out.Usage = u
			out.HasData = true
		}
		return out
	}
	c0, _ := cands[0].(map[string]any)
	if c0 == nil {
		return out
	}
	content, _ := c0["content"].(map[string]any)
	for _, p := range asSlice(content["parts"]) {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		if fc, ok := pm["functionCall"].(map[string]any); ok && fc != nil {
			name, _ := fc["name"].(string)
			id, _ := fc["id"].(string)
			args := map[string]any{}
			switch a := fc["args"].(type) {
			case map[string]any:
				args = a
			}
			if id == "" {
				id = "call_" + uuid.NewString()[:8]
			}
			out.ToolCalls = append(out.ToolCalls, ParsedToolCall{ID: sanitizeID(id, name), Name: name, Arguments: args})
			out.HasData = true
			continue
		}
		if t, ok := pm["text"].(string); ok && t != "" {
			if th, _ := pm["thought"].(bool); th {
				out.Thoughts = append(out.Thoughts, t)
			} else {
				out.Texts = append(out.Texts, t)
			}
			out.HasData = true
		}
	}
	if fr, _ := c0["finishReason"].(string); fr != "" {
		out.Finish = fr
		out.HasData = true
	}
	if u := parseUsage(resp["usageMetadata"]); u.HasUsage {
		out.Usage = u
		out.HasData = true
	}
	return out
}

func parseUsage(v any) Usage {
	m, _ := v.(map[string]any)
	if m == nil {
		return Usage{}
	}
	f := func(k string) int {
		switch n := m[k].(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
		return 0
	}
	prompt := f("promptTokenCount")
	cached := f("cachedContentTokenCount")
	cand := f("candidatesTokenCount")
	thoughts := f("thoughtsTokenCount")
	total := f("totalTokenCount")
	if prompt == 0 && cand == 0 && total == 0 && thoughts == 0 && cached == 0 {
		return Usage{}
	}
	return Usage{
		Input: prompt - cached, Output: cand + thoughts,
		CacheRead: cached, Reasoning: thoughts, Total: total, HasUsage: true,
	}
}

// ScanSSE reads an Antigravity SSE stream (data: lines) and calls fn per chunk.
func ScanSSE(r io.Reader, fn func(ParsedChunk)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		fn(ParseChunk(strings.TrimPrefix(line, "data:")))
	}
	return sc.Err()
}

// OpenAIChunks renders parsed output as OpenAI chat-completion SSE frames.
func OpenAIChunks(model string, chunks []ParsedChunk, startID string) (frames []string, text, reasoning string, prompt, completion, cacheRead, reasoningTok, total int, finish string) {
	if startID == "" {
		startID = "chatcmpl-" + uuid.NewString()[:8]
	}
	created := 0
	_ = created
	roleSent := false
	toolIndex := map[string]int{}
	nextToolIdx := 0
	finish = "stop"
	for _, c := range chunks {
		for _, t := range c.Texts {
			text += t
			h := map[string]any{
				"id": startID, "object": "chat.completion.chunk", "created": 0,
				"model":   model,
				"choices": []any{map[string]any{"index": 0, "delta": deltaWithRole(map[string]any{"content": t}, &roleSent), "finish_reason": nil}},
			}
			frames = append(frames, "data: "+mustJSON(h))
		}
		for _, t := range c.Thoughts {
			reasoning += t
		}
		for _, tc := range c.ToolCalls {
			idx, ok := toolIndex[tc.ID]
			if !ok {
				idx = nextToolIdx
				toolIndex[tc.ID] = idx
				nextToolIdx++
			}
			args, _ := json.Marshal(tc.Arguments)
			h := map[string]any{
				"id": startID, "object": "chat.completion.chunk", "created": 0,
				"model": model,
				"choices": []any{map[string]any{"index": 0, "delta": deltaWithRole(map[string]any{
					"tool_calls": []any{map[string]any{
						"index": idx, "id": tc.ID,
						"type":     "function",
						"function": map[string]any{"name": tc.Name, "arguments": string(args)},
					}},
				}, &roleSent), "finish_reason": nil}},
			}
			frames = append(frames, "data: "+mustJSON(h))
		}
		if c.Usage.HasUsage {
			prompt = c.Usage.Input
			completion = c.Usage.Output
			cacheRead = c.Usage.CacheRead
			reasoningTok = c.Usage.Reasoning
			total = c.Usage.Total
		}
		if c.Finish != "" && !strings.HasPrefix(c.Finish, "error:") {
			finish = mapFinish(c.Finish, len(toolIndex) > 0)
		}
	}
	if len(toolIndex) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	end := map[string]any{
		"id": startID, "object": "chat.completion.chunk", "created": 0,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	frames = append(frames, "data: "+mustJSON(end))
	return frames, text, reasoning, prompt, completion, cacheRead, reasoningTok, total, finish
}

func deltaWithRole(d map[string]any, sent *bool) map[string]any {
	if !*sent {
		d["role"] = "assistant"
		*sent = true
	}
	return d
}

func mapFinish(fr string, hasTools bool) string {
	switch fr {
	case "STOP":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// NonStreamOpenAI builds a full chat.completion JSON body from accumulated chunks.
func NonStreamOpenAI(model string, chunks []ParsedChunk) (body []byte, text string, prompt, completion, total int, finish string) {
	var texts []string
	var toolCalls []map[string]any
	prompt, completion, total = 0, 0, 0
	finish = "stop"
	for _, c := range chunks {
		texts = append(texts, c.Texts...)
		for _, tc := range c.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			toolCalls = append(toolCalls, map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": string(args)},
			})
		}
		if c.Usage.HasUsage {
			prompt = c.Usage.Input
			completion = c.Usage.Output
			total = c.Usage.Total
		}
		if c.Finish != "" && !strings.HasPrefix(c.Finish, "error:") {
			finish = mapFinish(c.Finish, false)
		}
	}
	text = strings.Join(texts, "")
	if len(toolCalls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	msg := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id": "chatcmpl-" + uuid.NewString()[:12], "object": "chat.completion",
		"created": 0, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total},
	}
	b, _ := json.Marshal(out)
	_ = fmt.Sprintf
	return b, text, prompt, completion, total, finish
}
