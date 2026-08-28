package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultAnthropicMaxTokens fills Anthropic's REQUIRED max_tokens when an
// OpenAI-shaped request omits it. Configurable to match deployment needs.
var DefaultAnthropicMaxTokens = 4096

// Anthropic -> OpenAI and OpenAI -> Anthropic
// Updated to handle reasoning effort (newer Anthropic) vs budget_tokens (legacy)

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type AnthropicThinking struct {
	Type         string  `json:"type"`                    // enabled | disabled
	Effort       *string `json:"effort,omitempty"`        // newer: low, medium, high, xhigh, max
	BudgetTokens *int    `json:"budget_tokens,omitempty"` // legacy
}

type AnthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	System        interface{}        `json:"system,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences interface{}        `json:"stop_sequences,omitempty"`
	Tools         []json.RawMessage  `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Thinking      *AnthropicThinking `json:"thinking,omitempty"`
}

type OpenAIMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content,omitempty"`
	ToolCalls  interface{} `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
}

type OpenAIChatRequest struct {
	Model             string            `json:"model"`
	Messages          []OpenAIMessage   `json:"messages"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Stop              interface{}       `json:"stop,omitempty"`
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        interface{}       `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   *string           `json:"reasoning_effort,omitempty"` // low, medium, high, etc. - OpenAI chat
	Reasoning         *OpenAIReasoning  `json:"reasoning,omitempty"`        // alternative field some clients use
	ResponseFormat    json.RawMessage   `json:"response_format,omitempty"`  // preserve structured outputs
	FrequencyPenalty  *float64          `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64          `json:"presence_penalty,omitempty"`
	Seed              *int64            `json:"seed,omitempty"`
}

type OpenAIReasoning struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// ---------- helpers: image / content block conversion ----------

func parseDataURI(uri string) (mediaType, data string, isData bool) {
	if !strings.HasPrefix(uri, "data:") {
		return "", "", false
	}
	// data:image/jpeg;base64,XXXX
	comma := strings.Index(uri, ",")
	if comma < 0 {
		return "", "", false
	}
	meta := uri[5:comma] // after "data:"
	data = uri[comma+1:]
	// meta like "image/jpeg;base64" or "image/png;base64"
	semi := strings.Index(meta, ";")
	if semi >= 0 {
		mediaType = meta[:semi]
	} else {
		mediaType = meta
	}
	if mediaType == "" {
		mediaType = "image/jpeg"
	}
	// validate base64
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		if _, err2 := base64.RawStdEncoding.DecodeString(data); err2 != nil {
			// not base64 - treat as opaque
		}
	}
	return mediaType, data, true
}

func openAIImageBlockToAnthropic(block map[string]interface{}) map[string]interface{} {
	// OpenAI: {"type":"image_url","image_url":{"url":"...","detail":"auto"}}
	if block["type"] == "image_url" {
		if iu, ok := block["image_url"].(map[string]interface{}); ok {
			if url, ok := iu["url"].(string); ok && url != "" {
				if mt, d, ok := parseDataURI(url); ok {
					return map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": mt,
							"data":       d,
						},
					}
				}
				// non-data URL: anthropic now supports url type, older needs base64
				// emit url source for forward-compat
				return map[string]interface{}{
					"type": "image",
					"source": map[string]interface{}{
						"type": "url",
						"url":  url,
					},
				}
			}
		}
		// fallback try direct url field
		if url, ok := block["url"].(string); ok {
			if mt, d, ok := parseDataURI(url); ok {
				return map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": mt, "data": d}}
			}
			return map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": url}}
		}
	}
	// already anthropic image? keep
	if block["type"] == "image" {
		return block
	}
	return block
}

func anthropicImageBlockToOpenAI(block map[string]interface{}) map[string]interface{} {
	// Anthropic: {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"..."}}
	if block["type"] == "image" {
		if src, ok := block["source"].(map[string]interface{}); ok {
			if src["type"] == "base64" {
				mt, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				if mt == "" {
					mt = "image/jpeg"
				}
				uri := "data:" + mt + ";base64," + data
				return map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": uri,
					},
				}
			}
			if src["type"] == "url" {
				if url, ok := src["url"].(string); ok {
					return map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}}
				}
			}
		}
	}
	// text etc keep
	return block
}

func normalizeOpenAIContentForAnthropic(content interface{}) interface{} {
	if content == nil {
		return nil
	}
	b, _ := json.Marshal(content)
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	var arr []interface{}
	if json.Unmarshal(b, &arr) == nil {
		out := make([]interface{}, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				// handle input_text alias already
				if m["type"] == "input_text" {
					m["type"] = "text"
				}
				if m["type"] == "image_url" || m["type"] == "image" {
					out = append(out, openAIImageBlockToAnthropic(m))
					continue
				}
				// text blocks keep
				out = append(out, m)
			} else {
				out = append(out, item)
			}
		}
		return out
	}
	var m map[string]interface{}
	if json.Unmarshal(b, &m) == nil {
		if m["type"] == "image_url" || m["type"] == "image" {
			return openAIImageBlockToAnthropic(m)
		}
		if m["type"] == "input_text" {
			m["type"] = "text"
		}
		return m
	}
	return content
}

func normalizeAnthropicContentForOpenAI(content interface{}) interface{} {
	if content == nil {
		return nil
	}
	b, _ := json.Marshal(content)
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	var arr []interface{}
	if json.Unmarshal(b, &arr) == nil {
		out := make([]interface{}, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == "image" {
					out = append(out, anthropicImageBlockToOpenAI(m))
					continue
				}
				// anthropic text blocks are compatible with openai text blocks
				out = append(out, m)
			} else {
				out = append(out, item)
			}
		}
		return out
	}
	var m map[string]interface{}
	if json.Unmarshal(b, &m) == nil {
		if m["type"] == "image" {
			return anthropicImageBlockToOpenAI(m)
		}
		return m
	}
	return content
}

// AnthropicToOpenAI converts anthropic messages request to OpenAI chat request
func AnthropicToOpenAI(body []byte) ([]byte, string, error) {
	var aReq AnthropicRequest
	if err := json.Unmarshal(body, &aReq); err != nil {
		return nil, "", err
	}
	var messages []OpenAIMessage
	if aReq.System != nil {
		switch v := aReq.System.(type) {
		case string:
			if v != "" {
				messages = append(messages, OpenAIMessage{Role: "system", Content: v})
			}
		case []interface{}:
			b, _ := json.Marshal(v)
			var blocks []map[string]interface{}
			json.Unmarshal(b, &blocks)
			var sysParts []string
			for _, bl := range blocks {
				if bl["type"] == "text" {
					if t, ok := bl["text"].(string); ok && t != "" {
						sysParts = append(sysParts, t)
					}
				}
			}
			sysText := strings.Join(sysParts, "\n\n")
			if sysText != "" {
				messages = append(messages, OpenAIMessage{Role: "system", Content: sysText})
			}
		default:
			b, _ := json.Marshal(v)
			var s string
			if json.Unmarshal(b, &s) == nil && s != "" {
				messages = append(messages, OpenAIMessage{Role: "system", Content: s})
			}
		}
	}
	for _, m := range aReq.Messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		content := m.Content
		// translate message content: handle tool_use / tool_result / image blocks
		content = translateAnthropicMessageContentToOpenAI(role, content)
		msg := OpenAIMessage{Role: role, Content: content}
		// special handling: anthropic assistant with tool_use already converted via content helper
		// but we also need to extract tool_calls from content array into top-level tool_calls
		// translateAnthropicMessageContentToOpenAI does that and returns either string or array;
		// for assistant with tool_use we converted to OpenAI assistant with tool_calls,
		// however the helper below returns the full message content transformation.
		// We need to detect if helper produced a map with tool_calls extraction
		if role == "assistant" {
			if conv, tc := extractAnthropicToolUseToOpenAI(content); tc != nil {
				msg.Content = conv
				msg.ToolCalls = tc
			}
		}
		// user with tool_result -> becomes multiple tool messages per entry
		// extractAnthropicToolResultToOpenAI expands one user message into several tool messages
		if role == "user" {
			if expanded := expandAnthropicToolResultToOpenAI(content); expanded != nil {
				// expanded is []OpenAIMessage (tool messages); add textual user part if any
				for _, em := range expanded {
					messages = append(messages, em)
				}
				// if there was also a text part in same message, extractAnthropic should have left it
				// we already handled tool_result extraction; skip adding the original
				// check if there was a non-tool text remaining
				if remaining := remainingAnthropicTextContent(content); remaining != nil {
					msg.Content = remaining
					// only add if there is text
					if s, ok := remaining.(string); !ok || s != "" {
						if arr, ok := remaining.([]interface{}); !ok || len(arr) > 0 {
							messages = append(messages, msg)
						}
					}
				}
				continue
			}
		}
		messages = append(messages, msg)
	}
	oReq := OpenAIChatRequest{
		Model:       aReq.Model,
		Messages:    messages,
		Stream:      aReq.Stream,
		Temperature: aReq.Temperature,
		Tools:       translateAnthropicToolsToOpenAI(aReq.Tools),
		TopP:        aReq.TopP,
	}
	// map stop_sequences -> stop
	if aReq.StopSequences != nil {
		oReq.Stop = aReq.StopSequences
	}
	// preserve anthropic tool_choice -> openai tool_choice
	if len(aReq.ToolChoice) > 0 && string(aReq.ToolChoice) != "null" {
		var tc map[string]interface{}
		if json.Unmarshal(aReq.ToolChoice, &tc) == nil {
			if typ, ok := tc["type"].(string); ok {
				switch typ {
				case "auto":
					oReq.ToolChoice = "auto"
				case "any":
					oReq.ToolChoice = "required"
				case "tool":
					if name, ok := tc["name"].(string); ok && name != "" {
						oReq.ToolChoice = map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
					} else {
						oReq.ToolChoice = aReq.ToolChoice
					}
				case "none":
					oReq.ToolChoice = "none"
				default:
					oReq.ToolChoice = aReq.ToolChoice
				}
			} else {
				oReq.ToolChoice = aReq.ToolChoice
			}
		} else {
			oReq.ToolChoice = aReq.ToolChoice
		}
	}
	if aReq.MaxTokens != 0 {
		oReq.MaxTokens = &aReq.MaxTokens
	}
	// Map Anthropic thinking -> OpenAI reasoning_effort
	if aReq.Thinking != nil && aReq.Thinking.Type == "enabled" {
		if aReq.Thinking.Effort != nil {
			oReq.ReasoningEffort = aReq.Thinking.Effort
		} else if aReq.Thinking.BudgetTokens != nil {
			// legacy budget -> map to effort heuristic
			effort := budgetToEffort(*aReq.Thinking.BudgetTokens)
			oReq.ReasoningEffort = &effort
		}
	}
	out, err := json.Marshal(oReq)
	return out, aReq.Model, err
}

// helpers for Anthropic -> OpenAI message translation

func translateAnthropicMessageContentToOpenAI(role string, content interface{}) interface{} {
	if content == nil {
		return nil
	}
	// string stays string
	b, _ := json.Marshal(content)
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	// content is array of blocks; normalize image blocks
	var arr []interface{}
	if json.Unmarshal(b, &arr) == nil {
		out := make([]interface{}, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				// keep tool_use/tool_result for handled elsewhere but normalize image
				if m["type"] == "image" {
					out = append(out, anthropicImageBlockToOpenAI(m))
					continue
				}
				out = append(out, m)
			} else {
				out = append(out, item)
			}
		}
		return out
	}
	return content
}

func extractAnthropicToolUseToOpenAI(content interface{}) (interface{}, interface{}) {
	b, _ := json.Marshal(content)
	var arr []interface{}
	if json.Unmarshal(b, &arr) != nil {
		return nil, nil
	}
	var textParts []string
	var toolCalls []map[string]interface{}
	var remaining []interface{}
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			switch m["type"] {
			case "tool_use":
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				input := m["input"]
				if input == nil {
					input = map[string]interface{}{}
				}
				argBytes, _ := json.Marshal(input)
				// if input is already string, keep
				var argStr string
				if s, ok := input.(string); ok {
					argStr = s
				} else {
					argStr = string(argBytes)
				}
				if id == "" {
					id = "call_" + name
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": argStr,
					},
				})
			case "text":
				if t, ok := m["text"].(string); ok {
					textParts = append(textParts, t)
				}
				remaining = append(remaining, m)
			case "image":
				remaining = append(remaining, anthropicImageBlockToOpenAI(m))
			default:
				remaining = append(remaining, m)
			}
		}
	}
	if len(toolCalls) == 0 {
		return nil, nil
	}
	var contentOut interface{}
	if len(textParts) > 0 {
		joined := strings.Join(textParts, "\n")
		// if there were only text+tool_use, OpenAI assistant content should be the text (or null)
		contentOut = joined
	} else {
		// no text, content should be empty or null but include tool_calls
		contentOut = nil
	}
	// if there were image blocks besides tool_use, need to keep them as content array
	if len(remaining) > 0 && len(toolCalls) > 0 {
		// mixed case with images — keep as array
		contentOut = remaining
	}
	return contentOut, toolCalls
}

func expandAnthropicToolResultToOpenAI(content interface{}) []OpenAIMessage {
	b, _ := json.Marshal(content)
	var arr []interface{}
	if json.Unmarshal(b, &arr) != nil {
		return nil
	}
	var out []OpenAIMessage
	hasToolResult := false
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok && m["type"] == "tool_result" {
			hasToolResult = true
			break
		}
	}
	if !hasToolResult {
		return nil
	}
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok && m["type"] == "tool_result" {
			toolUseID, _ := m["tool_use_id"].(string)
			c := m["content"]
			var contentStr string
			if c == nil {
				contentStr = ""
			} else if s, ok := c.(string); ok {
				contentStr = s
			} else if arr2, ok := c.([]interface{}); ok {
				// anthropic tool_result content can be array of text blocks
				var parts []string
				for _, p := range arr2 {
					if pm, ok := p.(map[string]interface{}); ok {
						if pm["type"] == "text" {
							if t, ok := pm["text"].(string); ok {
								parts = append(parts, t)
							}
						}
					} else if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				contentStr = strings.Join(parts, "\n")
			} else {
				bb, _ := json.Marshal(c)
				contentStr = string(bb)
			}
			out = append(out, OpenAIMessage{
				Role:       "tool",
				ToolCallID: toolUseID,
				Content:    contentStr,
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func remainingAnthropicTextContent(content interface{}) interface{} {
	b, _ := json.Marshal(content)
	var arr []interface{}
	if json.Unmarshal(b, &arr) != nil {
		return nil
	}
	var remaining []interface{}
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			if m["type"] == "tool_result" {
				continue
			}
			if m["type"] == "image" {
				remaining = append(remaining, anthropicImageBlockToOpenAI(m))
				continue
			}
			remaining = append(remaining, m)
		}
	}
	if len(remaining) == 0 {
		return nil
	}
	if len(remaining) == 1 {
		if m, ok := remaining[0].(map[string]interface{}); ok && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return remaining
}

// OpenAIToAnthropic converts OpenAI chat request to Anthropic
func OpenAIToAnthropic(body []byte) ([]byte, string, error) {
	var oReq OpenAIChatRequest
	if err := json.Unmarshal(body, &oReq); err != nil {
		return nil, "", err
	}
	var systemParts []string
	var system interface{}
	var messages []AnthropicMessage
	for _, m := range oReq.Messages {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok && s != "" {
				systemParts = append(systemParts, s)
			} else {
				b, _ := json.Marshal(m.Content)
				var s string
				if json.Unmarshal(b, &s) == nil && s != "" {
					systemParts = append(systemParts, s)
				} else if len(b) > 2 {
					// try to extract text from array blocks
					var arr []interface{}
					if json.Unmarshal(b, &arr) == nil {
						for _, item := range arr {
							if mm, ok := item.(map[string]interface{}); ok {
								if mm["type"] == "text" {
									if t, ok := mm["text"].(string); ok && t != "" {
										systemParts = append(systemParts, t)
									}
								}
							}
						}
					} else {
						systemParts = append(systemParts, string(b))
					}
				}
			}
			continue
		}
		// handle tool role -> anthropic tool_result
		if m.Role == "tool" {
			toolUseID := m.ToolCallID
			if toolUseID == "" {
				toolUseID = m.Name
			}
			var contentStr string
			if s, ok := m.Content.(string); ok {
				contentStr = s
			} else if m.Content != nil {
				bb, _ := json.Marshal(m.Content)
				var s2 string
				if json.Unmarshal(bb, &s2) == nil {
					contentStr = s2
				} else {
					contentStr = string(bb)
				}
			}
			// Anthropic expects tool_result inside user message
			// group consecutive tool messages into one user message with multiple tool_result blocks
			block := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     contentStr,
			}
			// if last message is already user with tool_result, append
			if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
				last := messages[len(messages)-1]
				// check if last content is array with tool_result
				b2, _ := json.Marshal(last.Content)
				var arr []interface{}
				if json.Unmarshal(b2, &arr) == nil {
					hasToolResult := false
					for _, it := range arr {
						if mm, ok := it.(map[string]interface{}); ok && mm["type"] == "tool_result" {
							hasToolResult = true
							break
						}
					}
					if hasToolResult {
						arr = append(arr, block)
						messages[len(messages)-1].Content = arr
						continue
					}
				}
			}
			messages = append(messages, AnthropicMessage{Role: "user", Content: []interface{}{block}})
			continue
		}
		// handle assistant with tool_calls -> anthropic tool_use
		if m.Role == "assistant" && m.ToolCalls != nil {
			converted := convertOpenAIAssistantToAnthropic(m)
			messages = append(messages, converted)
			continue
		}
		// normal message: handle image conversion
		content := normalizeOpenAIContentForAnthropic(m.Content)
		messages = append(messages, AnthropicMessage{Role: m.Role, Content: content})
	}
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	aReq := AnthropicRequest{
		Model:       oReq.Model,
		Messages:    messages,
		System:      system,
		Stream:      oReq.Stream,
		Temperature: oReq.Temperature,
		TopP:        oReq.TopP,
		Tools:       translateOpenAIToolsToAnthropic(oReq.Tools),
	}
	if oReq.Stop != nil {
		aReq.StopSequences = oReq.Stop
	}
	// preserve tool_choice if present (translate OpenAI -> Anthropic)
	if oReq.ToolChoice != nil {
		if b, err := json.Marshal(oReq.ToolChoice); err == nil && len(b) > 0 && string(b) != "null" {
			// handle string tool_choice
			var s string
			if json.Unmarshal(b, &s) == nil {
				switch s {
				case "auto":
					aReq.ToolChoice = json.RawMessage(`{"type":"auto"}`)
				case "required", "any":
					aReq.ToolChoice = json.RawMessage(`{"type":"any"}`)
				case "none":
					aReq.ToolChoice = json.RawMessage(`{"type":"none"}`)
				default:
					aReq.ToolChoice = b
				}
			} else {
				aReq.ToolChoice = b
			}
		}
	}
	// max_tokens handling: prefer MaxTokens, fallback MaxOutputTokens
	if oReq.MaxTokens != nil {
		aReq.MaxTokens = *oReq.MaxTokens
	} else if oReq.MaxOutputTokens != nil {
		aReq.MaxTokens = *oReq.MaxOutputTokens
	}
	if aReq.MaxTokens == 0 {
		// Anthropic requires max_tokens; OpenAI semantics leave it unset.
		// A modest default avoids the old surprise of outputs truncated at
		// 1024 tokens, while staying far below any model's maximum. Callers
		// wanting full-length generations should still set max_tokens.
		aReq.MaxTokens = DefaultAnthropicMaxTokens
	}
	// Map OpenAI reasoning_effort -> Anthropic thinking (effort, not budget)
	if oReq.ReasoningEffort != nil {
		effort := strings.ToLower(strings.TrimSpace(*oReq.ReasoningEffort))
		if effort != "" && effort != "none" && effort != "disabled" {
			aReq.Thinking = &AnthropicThinking{Type: "enabled", Effort: &effort}
		} else if effort == "none" || effort == "disabled" {
			// explicitly disabled
			aReq.Thinking = &AnthropicThinking{Type: "disabled"}
		}
	} else if oReq.Reasoning != nil && oReq.Reasoning.Effort != nil {
		effort := strings.ToLower(strings.TrimSpace(*oReq.Reasoning.Effort))
		if effort != "" && effort != "none" {
			aReq.Thinking = &AnthropicThinking{Type: "enabled", Effort: &effort}
		}
	}
	out, err := json.Marshal(aReq)
	return out, oReq.Model, err
}

func convertOpenAIAssistantToAnthropic(m OpenAIMessage) AnthropicMessage {
	var contentBlocks []interface{}
	// content may be string or array
	if s, ok := m.Content.(string); ok && s != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": s})
	} else if m.Content != nil {
		b, _ := json.Marshal(m.Content)
		var s2 string
		var arr []interface{}
		if json.Unmarshal(b, &s2) == nil && s2 != "" {
			contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": s2})
		} else if json.Unmarshal(b, &arr) == nil {
			for _, item := range arr {
				if mm, ok := item.(map[string]interface{}); ok {
					if mm["type"] == "image_url" {
						contentBlocks = append(contentBlocks, openAIImageBlockToAnthropic(mm))
					} else if mm["type"] == "text" {
						contentBlocks = append(contentBlocks, mm)
					} else {
						contentBlocks = append(contentBlocks, mm)
					}
				}
			}
		} else if len(b) > 2 && string(b) != "null" {
			contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": string(b)})
		}
	}
	// tool_calls -> tool_use blocks
	if m.ToolCalls != nil {
		b, _ := json.Marshal(m.ToolCalls)
		var tcs []map[string]interface{}
		if json.Unmarshal(b, &tcs) == nil {
			for _, tc := range tcs {
				id, _ := tc["id"].(string)
				fn, _ := tc["function"].(map[string]interface{})
				if fn == nil {
					// try marshal
					bb, _ := json.Marshal(tc["function"])
					json.Unmarshal(bb, &fn)
				}
				name, _ := fn["name"].(string)
				argsRaw, _ := fn["arguments"]
				var input interface{}
				if s, ok := argsRaw.(string); ok && s != "" {
					if json.Unmarshal([]byte(s), &input) != nil {
						input = map[string]interface{}{"raw": s}
					}
				} else if argsRaw != nil {
					input = argsRaw
				} else {
					input = map[string]interface{}{}
				}
				if input == nil {
					input = map[string]interface{}{}
				}
				if id == "" {
					id = "toolu_" + name
				}
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": input,
				})
			}
		}
	}
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": ""})
	}
	return AnthropicMessage{Role: "assistant", Content: contentBlocks}
}

func normalizeContentInputTextToText(content interface{}) interface{} {
	if content == nil {
		return content
	}
	b, _ := json.Marshal(content)
	// If content is string, keep as is
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	// If content is array of blocks
	var arr []interface{}
	if json.Unmarshal(b, &arr) == nil {
		for i, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == "input_text" {
					m["type"] = "text"
					arr[i] = m
				} else if m["type"] == "image_url" || m["type"] == "input_image" {
					// normalize image block
					if m["type"] == "input_image" {
						// Responses input_image -> openai image_url
						if iu, ok := m["image_url"]; ok {
							arr[i] = map[string]interface{}{"type": "image_url", "image_url": iu}
						} else {
							m["type"] = "image_url"
							arr[i] = m
						}
					}
				}
				// Also handle text field is already text, keep
			}
		}
		return arr
	}
	// Single block object
	var m map[string]interface{}
	if json.Unmarshal(b, &m) == nil {
		if m["type"] == "input_text" {
			m["type"] = "text"
			return m
		}
		if m["type"] == "input_image" {
			if iu, ok := m["image_url"]; ok {
				return map[string]interface{}{"type": "image_url", "image_url": iu}
			}
			m["type"] = "image_url"
			return m
		}
	}
	return content
}

func budgetToEffort(budget int) string {
	if budget < 5000 {
		return "low"
	}
	if budget < 15000 {
		return "medium"
	}
	if budget < 30000 {
		return "high"
	}
	return "max"
}

func openAIToolToAnthropic(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	// OpenAI format: {"type":"function","function":{"name":"...","description":"...","parameters":{...}}}
	// Also sometimes direct {"name":...} (already anthropic) — detect
	if _, hasFunc := m["function"]; hasFunc {
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			// try to parse function as raw
			b, _ := json.Marshal(m["function"])
			json.Unmarshal(b, &fn)
		}
		if fn != nil {
			name, _ := fn["name"].(string)
			desc, _ := fn["description"].(string)
			params := fn["parameters"]
			if name == "" {
				return raw, false
			}
			out := map[string]any{
				"name": name,
			}
			if desc != "" {
				out["description"] = desc
			}
			if params != nil {
				out["input_schema"] = params
			} else {
				out["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			b, _ := json.Marshal(out)
			return json.RawMessage(b), true
		}
	}
	// If it already looks like anthropic (has name and input_schema), keep as is
	if _, hasName := m["name"]; hasName {
		if _, hasSchema := m["input_schema"]; hasSchema {
			return raw, true
		}
		// OpenAI without wrapper? {"name":"...","parameters":...} -> anthropic
		if params, ok := m["parameters"]; ok {
			out := map[string]any{
				"name": m["name"],
			}
			if d, ok := m["description"].(string); ok && d != "" {
				out["description"] = d
			}
			out["input_schema"] = params
			b, _ := json.Marshal(out)
			return json.RawMessage(b), true
		}
	}
	return raw, false
}

func anthropicToolToOpenAI(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	// Anthropic: {"name":"...","description":"...","input_schema":{...}}
	if name, hasName := m["name"]; hasName {
		if _, hasFunc := m["function"]; !hasFunc {
			// Convert to OpenAI
			desc, _ := m["description"].(string)
			schema := m["input_schema"]
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  schema,
				},
			}
			// Preserve other fields like strict?
			b, _ := json.Marshal(out)
			return json.RawMessage(b), true
		}
	}
	return raw, false
}

func translateOpenAIToolsToAnthropic(tools []json.RawMessage) []json.RawMessage {
	if len(tools) == 0 {
		return tools
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		if conv, ok := openAIToolToAnthropic(t); ok {
			out = append(out, conv)
		} else {
			out = append(out, t)
		}
	}
	return out
}

func translateAnthropicToolsToOpenAI(tools []json.RawMessage) []json.RawMessage {
	if len(tools) == 0 {
		return tools
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		if conv, ok := anthropicToolToOpenAI(t); ok {
			out = append(out, conv)
		} else {
			out = append(out, t)
		}
	}
	return out
}

// Responses -> Chat translation
type ResponsesRequest struct {
	Model        string           `json:"model"`
	Input        interface{}      `json:"input"`
	Instructions interface{}      `json:"instructions,omitempty"`
	Stream       bool             `json:"stream,omitempty"`
	Temperature  *float64         `json:"temperature,omitempty"`
	TopP         *float64         `json:"top_p,omitempty"`
	MaxTokens    *int             `json:"max_output_tokens,omitempty"`
	Reasoning    *OpenAIReasoning `json:"reasoning,omitempty"`
	// Agent-loop + tool fields: dropped silently before this fix.
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	// also support direct reasoning_effort for compatibility
	ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
	ResponseFormat  json.RawMessage `json:"response_format,omitempty"`
	Text            json.RawMessage `json:"text,omitempty"` // Responses text.format
}

func ResponsesToChat(body []byte) ([]byte, string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", err
	}
	var rReq ResponsesRequest
	if err := json.Unmarshal(body, &rReq); err != nil {
		return nil, "", err
	}
	// Handle case where client sends "messages" instead of "input" for responses (common with some SDKs)
	if rReq.Input == nil {
		if msgRaw, hasMsg := raw["messages"]; hasMsg && len(msgRaw) > 2 {
			var msgs []interface{}
			if json.Unmarshal(msgRaw, &msgs) == nil && len(msgs) > 0 {
				rReq.Input = msgs
			} else {
				rReq.Input = msgRaw
			}
		}
	}
	var messages []OpenAIMessage
	if rReq.Instructions != nil {
		switch v := rReq.Instructions.(type) {
		case string:
			if v != "" {
				messages = append(messages, OpenAIMessage{Role: "system", Content: v})
			}
		default:
			b, _ := json.Marshal(v)
			var s string
			if json.Unmarshal(b, &s) == nil && s != "" {
				messages = append(messages, OpenAIMessage{Role: "system", Content: s})
			} else if len(b) > 2 {
				messages = append(messages, OpenAIMessage{Role: "system", Content: string(b)})
			}
		}
	}
	switch v := rReq.Input.(type) {
	case string:
		if v != "" {
			messages = append(messages, OpenAIMessage{Role: "user", Content: v})
		}
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				// Agent-loop items that have no role/content of their own.
				// Before this fix they fell into the generic message branch
				// below and became user messages with nil content — the tool
				// call context vanished and tool results were silently
				// dropped from the conversation.
				switch itemType, _ := m["type"].(string); itemType {
				case "function_call":
					name, _ := m["name"].(string)
					if name == "" {
						continue
					}
					// Arguments may arrive as a JSON string (spec) or as an
					// already-parsed object (loose SDKs); stringifying the
					// latter beats silently dropping the real arguments.
					var args string
					if s, ok := m["arguments"].(string); ok {
						args = s
					} else if m["arguments"] != nil {
						if b, err := json.Marshal(m["arguments"]); err == nil {
							args = string(b)
						}
					}
					if args == "" {
						args = "{}"
					}
					callID, _ := m["call_id"].(string)
					messages = append(messages, OpenAIMessage{
						Role: "assistant",
						ToolCalls: []map[string]interface{}{{
							"id":       callID,
							"type":     "function",
							"function": map[string]interface{}{"name": name, "arguments": args},
						}},
					})
					continue
				case "function_call_output":
					callID, _ := m["call_id"].(string)
					messages = append(messages, OpenAIMessage{
						Role:       "tool",
						ToolCallID: callID,
						Content:    functionOutputToText(m["output"]),
					})
					continue
				case "reasoning", "item_reference":
					// No chat-format counterpart; forwarding them as user
					// messages injected null-content turns into the convo.
					continue
				}
				role, _ := m["role"].(string)
				content := m["content"]
				if role == "" {
					role = "user"
				}
				// Normalize content: convert input_text -> text for chat
				content = normalizeContentInputTextToText(content)
				messages = append(messages, OpenAIMessage{Role: role, Content: content})
			} else if s, ok := item.(string); ok && s != "" {
				messages = append(messages, OpenAIMessage{Role: "user", Content: s})
			}
		}
	default:
		if v != nil {
			b, _ := json.Marshal(v)
			var s string
			if json.Unmarshal(b, &s) == nil && s != "" {
				messages = append(messages, OpenAIMessage{Role: "user", Content: s})
			} else if len(b) > 2 && string(b) != "null" {
				// Try to handle single message object with input_text
				var single map[string]interface{}
				if json.Unmarshal(b, &single) == nil {
					if c, ok := single["content"]; ok {
						single["content"] = normalizeContentInputTextToText(c)
						role, _ := single["role"].(string)
						if role == "" {
							// Responses-API items (and some resume payloads) legitimately omit role;
							// treating them as user input beats a client-triggerable panic mid-SSE.
							role = "user"
						}
						messages = append(messages, OpenAIMessage{Role: role, Content: single["content"]})
					} else {
						messages = append(messages, OpenAIMessage{Role: "user", Content: string(b)})
					}
				} else {
					messages = append(messages, OpenAIMessage{Role: "user", Content: string(b)})
				}
			}
		}
	}
	if len(messages) == 0 {
		return nil, "", fmt.Errorf("input field required")
	}
	oReq := OpenAIChatRequest{
		Model:       rReq.Model,
		Messages:    messages,
		Stream:      rReq.Stream,
		Temperature: rReq.Temperature,
		TopP:        rReq.TopP,
		MaxTokens:   rReq.MaxTokens,
	}
	// Clients that send legacy max_tokens to /v1/responses still deserve a
	// working cap instead of a silently-unbounded generation.
	if oReq.MaxTokens == nil {
		if mtRaw, ok := raw["max_tokens"]; ok {
			var mt int
			if json.Unmarshal(mtRaw, &mt) == nil && mt > 0 {
				oReq.MaxTokens = &mt
			}
		}
	}
	if rReq.ParallelToolCalls != nil {
		oReq.ParallelToolCalls = rReq.ParallelToolCalls
	}
	// Responses uses a flat tool shape ({type,name,parameters}); chat uses
	// {type:"function",function:{...}}. Chat-shaped tools pass through.
	if len(rReq.Tools) > 0 {
		chatTools := make([]json.RawMessage, 0, len(rReq.Tools))
		for _, t := range rReq.Tools {
			if ct, ok := responsesToolToChat(t); ok {
				chatTools = append(chatTools, ct)
			}
		}
		if len(chatTools) > 0 {
			oReq.Tools = chatTools
		}
	}
	if len(rReq.ToolChoice) > 0 && string(rReq.ToolChoice) != "null" {
		var tcStr string
		if json.Unmarshal(rReq.ToolChoice, &tcStr) == nil {
			oReq.ToolChoice = tcStr
		} else {
			var tc struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(rReq.ToolChoice, &tc) == nil && tc.Type == "function" && tc.Name != "" {
				oReq.ToolChoice = map[string]interface{}{
					"type":     "function",
					"function": map[string]string{"name": tc.Name},
				}
			}
		}
	}
	// Preserve structured output: response_format or text.format
	if len(rReq.ResponseFormat) > 0 {
		oReq.ResponseFormat = rReq.ResponseFormat
	} else if len(rReq.Text) > 0 {
		var textObj map[string]json.RawMessage
		if json.Unmarshal(rReq.Text, &textObj) == nil {
			if fmtRaw, ok := textObj["format"]; ok && len(fmtRaw) > 0 && string(fmtRaw) != "null" {
				oReq.ResponseFormat = fmtRaw
			} else if len(rReq.Text) > 2 {
				oReq.ResponseFormat = rReq.Text
			}
		}
	}
	// Map Responses reasoning -> Chat reasoning_effort
	if rReq.Reasoning != nil && rReq.Reasoning.Effort != nil {
		oReq.ReasoningEffort = rReq.Reasoning.Effort
	} else if rReq.ReasoningEffort != nil {
		oReq.ReasoningEffort = rReq.ReasoningEffort
	}
	out, err := json.Marshal(oReq)
	return out, rReq.Model, err
}

// ResponsesToAnthropic converts Responses directly to Anthropic (via Chat)
func ResponsesToAnthropic(body []byte) ([]byte, string, error) {
	chatBody, model, err := ResponsesToChat(body)
	if err != nil {
		return nil, "", err
	}
	out, _, err := OpenAIToAnthropic(chatBody)
	return out, model, err
}

// responsesToolToChat converts one Responses-API tool definition to chat
// format. Flat function tools ({type:"function",name,…}) become nested
// ({type:"function",function:{…}}); already-chat-shaped tools pass through;
// non-function tool types (web_search, file_search, …) have no chat
// counterpart and are dropped rather than forwarded to a validator that
// 400s on unknown tool shapes.
func responsesToolToChat(t json.RawMessage) (json.RawMessage, bool) {
	var tm map[string]json.RawMessage
	if err := json.Unmarshal(t, &tm); err != nil {
		return nil, false
	}
	if _, nested := tm["function"]; nested {
		return t, true
	}
	var tf struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	}
	if err := json.Unmarshal(t, &tf); err != nil || tf.Type != "function" || tf.Name == "" {
		return nil, false
	}
	fn := map[string]interface{}{"name": tf.Name}
	if tf.Description != "" {
		fn["description"] = tf.Description
	}
	if len(tf.Parameters) > 0 && string(tf.Parameters) != "null" {
		fn["parameters"] = tf.Parameters
	}
	if tf.Strict {
		fn["strict"] = true
	}
	return mustJSON(map[string]interface{}{"type": "function", "function": fn}), true
}

// functionOutputToText flattens a function_call_output's output value into
// chat tool-message content: strings pass through; arrays of output_text
// parts are joined; anything else is preserved as its JSON encoding so no
// tool result is silently lost.
func functionOutputToText(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []interface{}:
		var texts []string
		sawTextPart := false
		for _, p := range t {
			if pm, ok := p.(map[string]interface{}); ok {
				if pt, _ := pm["type"].(string); pt == "output_text" {
					if txt, ok := pm["text"].(string); ok {
						texts = append(texts, txt)
						sawTextPart = true
						continue
					}
				}
			}
			sawTextPart = false
			break
		}
		if sawTextPart && len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// ExtractModel extracts model field from raw json quickly
func ExtractModel(body []byte) string {
	var tmp struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &tmp)
	return tmp.Model
}

func IsStreaming(body []byte) bool {
	var tmp struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &tmp)
	return tmp.Stream
}

func ExtractReasoningEffort(body []byte) string {
	var tmp struct {
		ReasoningEffort *string            `json:"reasoning_effort"`
		Reasoning       *OpenAIReasoning   `json:"reasoning"`
		Thinking        *AnthropicThinking `json:"thinking"`
	}
	json.Unmarshal(body, &tmp)
	if tmp.ReasoningEffort != nil {
		return *tmp.ReasoningEffort
	}
	if tmp.Reasoning != nil && tmp.Reasoning.Effort != nil {
		return *tmp.Reasoning.Effort
	}
	if tmp.Thinking != nil && tmp.Thinking.Effort != nil {
		return *tmp.Thinking.Effort
	}
	if tmp.Thinking != nil && tmp.Thinking.BudgetTokens != nil {
		return budgetToEffort(*tmp.Thinking.BudgetTokens)
	}
	return ""
}

func IsAnthropicRequest(body []byte) bool {
	return strings.Contains(string(body), "\"max_tokens\"") && strings.Contains(string(body), "\"messages\"") && !strings.Contains(string(body), "\"input\"")
}

// ---------- error envelope translation ----------

func TranslateAnthropicErrorToOpenAI(body []byte, status int) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	// Anthropic error shape: {"type":"error","error":{"type":"invalid_request_error","message":"..."}}
	if m["type"] == "error" {
		if inner, ok := m["error"].(map[string]interface{}); ok {
			msg, _ := inner["message"].(string)
			typ, _ := inner["type"].(string)
			if typ == "" {
				typ = "invalid_request_error"
			}
			if msg == "" {
				msg = "anthropic error"
			}
			out := map[string]interface{}{
				"error": map[string]interface{}{
					"message": msg,
					"type":    typ,
					"code":    status,
				},
			}
			if b, err := json.Marshal(out); err == nil {
				return b
			}
		}
	}
	// if it's already openai shape, keep
	if _, hasErr := m["error"]; hasErr {
		return body
	}
	return body
}

func TranslateOpenAIErrorToAnthropic(body []byte, status int) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	// OpenAI: {"error":{"message":"...","type":"invalid_request_error","code":400}}
	if inner, ok := m["error"].(map[string]interface{}); ok {
		msg, _ := inner["message"].(string)
		typ, _ := inner["type"].(string)
		if typ == "" {
			typ = "invalid_request_error"
		}
		if msg == "" {
			msg = "openai error"
		}
		out := map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    typ,
				"message": msg,
			},
		}
		if b, err := json.Marshal(out); err == nil {
			return b
		}
	}
	return body
}

// mustJSON marshals v or returns an empty JSON string on failure; only
// called with JSON-safe values built here.
func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}
