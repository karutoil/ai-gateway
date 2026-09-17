package devin

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FallbackModels mirrors the reference extension fallback when discovery is
// unavailable. Devin bills via seat/quota, so token costs are zero.
type FallbackModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
}

// PublicModels is the seed catalog for discovery.
var PublicModels = []FallbackModel{
	{ID: "swe-2", Name: "SWE-2", ContextWindow: 200000, MaxTokens: 64000},
	{ID: "swe-1-7", Name: "SWE-1.7", ContextWindow: 200000, MaxTokens: 64000},
	{ID: "swe-1-6", Name: "SWE-1.6", ContextWindow: 200000, MaxTokens: 64000},
}

// StripPrefix removes an optional "devin/" routing prefix.
func StripPrefix(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		if strings.EqualFold(strings.TrimSpace(model[:i]), "devin") {
			return strings.TrimSpace(model[i+1:])
		}
	}
	return strings.TrimSpace(model)
}

// FromOpenAI converts an OpenAI chat-completions body into a ChatInput.
// Anthropic/Responses callers normalize to chat JSON first.
func FromOpenAI(openAIJSON []byte, model string) (ChatInput, error) {
	var in map[string]any
	if err := json.Unmarshal(openAIJSON, &in); err != nil {
		return ChatInput{}, err
	}
	out := ChatInput{Model: StripPrefix(model), Temperature: -1}
	var system []string
	raw, _ := in["messages"].([]any)
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := openAIText(m["content"]); t != "" {
				system = append(system, t)
			}
		case "user":
			if t := openAIText(m["content"]); strings.TrimSpace(t) != "" {
				out.Messages = append(out.Messages, WireMessage{Role: WireRoleChat, Text: t})
			}
		case "assistant":
			// One OpenAI assistant turn is ONE wire prompt: text and its
			// tool calls travel together. Splitting them (text turn + one
			// turn per call) breaks the backend's turn tracking and the
			// follow-up tool calls come back malformed.
			wm := WireMessage{Role: WireRoleChat, Text: openAIText(m["content"])}
			for _, tc := range asSlice(m["tool_calls"]) {
				tcm, _ := tc.(map[string]any)
				if tcm == nil {
					continue
				}
				fn, _ := tcm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if strings.TrimSpace(name) == "" {
					continue
				}
				args := "{}"
				switch a := fn["arguments"].(type) {
				case string:
					if strings.TrimSpace(a) != "" {
						args = a
					}
				case map[string]any:
					if b, err := json.Marshal(a); err == nil {
						args = string(b)
					}
				}
				id, _ := tcm["id"].(string)
				if strings.TrimSpace(id) == "" {
					// The backend echoes ids back to pair results; an
					// empty id echoes as empty and breaks downstream
					// grouping for parallel calls.
					id = fmt.Sprintf("call_upstream_%d_%d", len(out.Messages), len(wm.ToolCalls))
				}
				wm.ToolCalls = append(wm.ToolCalls, WireToolCall{ID: id, Name: name, ArgumentsJSON: args})
			}
			if strings.TrimSpace(wm.Text) != "" || len(wm.ToolCalls) > 0 {
				out.Messages = append(out.Messages, wm)
			}
		case "tool":
			id, _ := m["tool_call_id"].(string)
			text := openAIText(m["content"])
			if strings.TrimSpace(text) == "" {
				text = "Tool finished."
			}
			out.Messages = append(out.Messages, WireMessage{Role: WireRoleTool, Text: text, ToolCallID: id})
		}
	}
	out.System = strings.Join(system, "\n\n")
	for _, item := range asSlice(in["tools"]) {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		params := "{}"
		if p := fn["parameters"]; p != nil {
			switch v := p.(type) {
			case string:
				// Loose SDKs send the schema pre-stringified; using it
				// verbatim preserves the object, while re-marshaling
				// would double-encode it into a string literal the
				// backend cannot parse as a schema.
				if s := strings.TrimSpace(v); s != "" && json.Valid([]byte(s)) {
					params = s
				}
			default:
				if b, err := json.Marshal(p); err == nil {
					params = string(b)
				}
			}
		}
		out.Tools = append(out.Tools, ToolDef{Name: name, Description: desc, Parameters: params})
	}
	if t, ok := in["temperature"]; ok {
		if f, ok := toFloat(t); ok {
			out.Temperature = f
		}
	}
	for _, k := range []string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
		if v, ok := in[k]; ok {
			if f, ok := toFloat(v); ok && f > 0 {
				out.MaxTokens = int(f)
				break
			}
		}
	}
	return out, nil
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// openAIText extracts plain text from string or content-array message content.
func openAIText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			typ, _ := m["type"].(string)
			// Responses-normalized histories replay assistant text as
			// output_text/refusal parts; dropping them loses the turn.
			if typ != "text" && typ != "input_text" && typ != "output_text" && typ != "refusal" {
				continue
			}
			if s, _ := m["text"].(string); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
