package antigravity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// BuildRequest converts an OpenAI chat-completions body into an Antigravity
// v1internal:streamGenerateContent body. Responses/Anthropic callers normalize
// to chat JSON first (translate.ResponsesToChat / AnthropicToOpenAI).
func BuildRequest(openAIJSON []byte, projectID, runtime, effort string) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(openAIJSON, &in); err != nil {
		return nil, fmt.Errorf("invalid openai body: %w", err)
	}
	publicModel, _ := in["model"].(string)
	publicModel = stripPrefix(publicModel)

	systemTexts, contents, toolNameByID := convertMessages(in, runtime)
	if len(systemTexts) == 0 {
		systemTexts = []string{"You are Antigravity, a powerful agentic AI coding assistant designed by Google DeepMind. You are pair programming with a user to solve coding tasks. Be concise, practical, and tool-aware."}
	}
	_ = toolNameByID

	request := map[string]any{
		"contents": contents,
		"systemInstruction": map[string]any{
			"role":  "user",
			"parts": textParts(systemTexts),
		},
	}
	gen := map[string]any{}
	if t, ok := in["temperature"]; ok {
		if f, ok := toFloat(t); ok {
			gen["temperature"] = f
		}
	}
	includeThoughts, budget := ThinkingBudget(runtime, effort)
	gen["thinkingConfig"] = map[string]any{
		"includeThoughts": includeThoughts,
		"thinkingBudget":  budget,
	}
	maxAllowed := MaxOutputTokens(publicModel, runtime)
	if m := firstFloat(in, "max_completion_tokens", "max_tokens", "max_output_tokens"); m > 0 {
		if int(m) < maxAllowed {
			gen["maxOutputTokens"] = int(m)
		} else {
			gen["maxOutputTokens"] = maxAllowed
		}
	} else {
		gen["maxOutputTokens"] = maxAllowed
	}
	if len(gen) > 0 {
		request["generationConfig"] = gen
	}
	if tools := convertTools(in); tools != nil {
		request["tools"] = tools
	}
	if tc := convertToolChoice(in); tc != nil {
		request["toolConfig"] = tc
	}

	isClaude := strings.HasPrefix(publicModel, "claude-") || strings.HasPrefix(runtime, "claude-")
	isNonGemini := isClaude || strings.HasPrefix(publicModel, "gpt-oss-") || strings.HasPrefix(runtime, "gpt-oss-") ||
		(!strings.HasPrefix(publicModel, "gemini-") && !strings.HasPrefix(runtime, "gemini-"))
	step := len(contents)
	if step < 1 {
		step = 1
	}
	traj := uuid.NewString()
	conv := uuid.NewString()
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	sessionID := fmt.Sprintf("%d", n.Int64())
	labels := map[string]string{
		"last_step_index":          fmt.Sprintf("%d", step-1),
		"request_id":               fmt.Sprintf("%s-0", traj),
		"trajectory_id":            traj,
		"used_claude":              boolStr(isClaude),
		"used_claude_conservative": boolStr(isClaude),
		"used_non_gemini_model":    boolStr(isNonGemini),
	}
	request["sessionId"] = sessionID
	request["labels"] = labels

	out := map[string]any{
		"project":     projectID,
		"model":       runtime,
		"request":     request,
		"requestType": "Agent",
		"userAgent":   "antigravity",
		"requestId":   fmt.Sprintf("agent/%s/%d/%s/%d", conv, time.Now().UnixMilli(), traj, step),
	}
	_ = publicModel
	return json.Marshal(out)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func stripPrefix(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		return model[i+1:]
	}
	return model
}

func textParts(texts []string) []map[string]any {
	out := make([]map[string]any, 0, len(texts))
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		out = append(out, map[string]any{"text": t})
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"text": " "})
	}
	return out
}

// requiresThoughtSignature mirrors pi-antigravity geminiRequiresThoughtSignature:
// Gemini 3+ runtimes reject historical functionCall parts without a valid
// thoughtSignature.
func requiresThoughtSignature(runtime string) bool {
	if !strings.HasPrefix(runtime, "gemini-") {
		return false
	}
	rest := strings.TrimPrefix(runtime, "gemini-")
	major := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		major = major*10 + int(r-'0')
	}
	return major >= 3
}

func isValidThoughtSignature(s string) bool {
	if s == "" || len(s)%4 != 0 {
		return false
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			continue
		}
		return false
	}
	return true
}

// extractSignature reads an optional thought signature from OpenAI tool_call
// shapes. Standard clients never send one; when present and valid it is
// preserved so signed histories keep working.
func extractSignature(tcm, fn map[string]any) string {
	for _, m := range []map[string]any{tcm, fn} {
		if m == nil {
			continue
		}
		for _, k := range []string{"thoughtSignature", "thought_signature", "thinkingSignature", "thinking_signature"} {
			if s, _ := m[k].(string); isValidThoughtSignature(s) {
				return s
			}
		}
	}
	return ""
}

// convertMessages maps OpenAI messages to Gemini contents + system texts.
func convertMessages(in map[string]any, runtime string) (system []string, contents []map[string]any, toolNames map[string]string) {
	toolNames = map[string]string{}
	requiresSig := requiresThoughtSignature(runtime)
	// droppedArgs tracks historical tool calls omitted for missing signatures
	// so their results become plain-text observations instead of orphaned
	// functionResponse parts (which the backend also rejects).
	droppedArgs := map[string]string{}
	raw, _ := in["messages"].([]any)
	// First pass: index assistant tool_call ids -> names.
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role != "assistant" {
			continue
		}
		for _, tc := range asSlice(m["tool_calls"]) {
			tcm, _ := tc.(map[string]any)
			if tcm == nil {
				continue
			}
			id, _ := tcm["id"].(string)
			fn, _ := tcm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if id != "" && name != "" {
				toolNames[id] = name
			}
		}
	}
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := stringContent(m["content"]); t != "" {
				system = append(system, t)
			}
		case "user":
			parts := userParts(m["content"])
			if len(parts) > 0 {
				appendTurn(&contents, "user", parts)
			}
		case "assistant":
			parts := []map[string]any{}
			if t := stringContent(m["content"]); strings.TrimSpace(t) != "" {
				parts = append(parts, map[string]any{"text": t})
			}
			// content array with text parts
			for _, p := range contentArrayTexts(m["content"]) {
				parts = append(parts, map[string]any{"text": p})
			}
			for _, tc := range asSlice(m["tool_calls"]) {
				tcm, _ := tc.(map[string]any)
				if tcm == nil {
					continue
				}
				id, _ := tcm["id"].(string)
				fn, _ := tcm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				args := map[string]any{}
				argsText := "{}"
				if s, _ := fn["arguments"].(string); s != "" {
					argsText = s
					_ = json.Unmarshal([]byte(s), &args)
				} else if am, ok := fn["arguments"].(map[string]any); ok {
					args = am
					if b, err := json.Marshal(am); err == nil {
						argsText = string(b)
					}
				}
				sig := extractSignature(tcm, fn)
				if requiresSig && sig == "" {
					// Unsigned history: omit the functionCall and convert the
					// later tool result into an observation text part.
					if id != "" {
						droppedArgs[id] = argsText
						droppedArgs[sanitizeID(id, name)] = argsText
					} else {
						droppedArgs["empty:"+name] = argsText
					}
					continue
				}
				call := map[string]any{"name": name, "args": args}
				if id != "" {
					// Claude/GPT-OSS bridge requires stable ids; keep OpenAI ids sanitized.
					call["id"] = sanitizeID(id, name)
				}
				if sig != "" {
					call["thoughtSignature"] = sig
				}
				parts = append(parts, map[string]any{"functionCall": call})
			}
			if len(parts) > 0 {
				appendTurn(&contents, "model", parts)
			}
		case "tool":
			toolCallID, _ := m["tool_call_id"].(string)
			name := toolNames[toolCallID]
			if name == "" {
				name, _ = m["name"].(string)
			}
			if name == "" {
				name = "tool"
			}
			text := stringContent(m["content"])
			if strings.TrimSpace(text) == "" {
				text = "Tool finished."
			}
			if requiresSig {
				if argsText, ok := droppedArgs[toolCallID]; ok || toolCallID == "" {
					if !ok {
						argsText, ok = droppedArgs["empty:"+name]
					}
					if ok {
						label := "`" + name + "`"
						if argsText != "" && argsText != "{}" {
							label = "`" + name + "` (" + argsText + ")"
						}
						appendTurn(&contents, "user", []map[string]any{{"text": "[Observation from " + label + ":\n" + text + "]"}})
						break
					}
				} else if _, ok := droppedArgs[sanitizeID(toolCallID, name)]; ok {
					argsText := droppedArgs[sanitizeID(toolCallID, name)]
					label := "`" + name + "`"
					if argsText != "" && argsText != "{}" {
						label = "`" + name + "` (" + argsText + ")"
					}
					appendTurn(&contents, "user", []map[string]any{{"text": "[Observation from " + label + ":\n" + text + "]"}})
					break
				}
			}
			resp := map[string]any{
				"functionResponse": map[string]any{
					"name":     name,
					"response": map[string]any{"output": text},
				},
			}
			if toolCallID != "" {
				resp["functionResponse"].(map[string]any)["id"] = sanitizeID(toolCallID, name)
			}
			appendTurn(&contents, "user", []map[string]any{resp})
		}
	}
	// Antigravity requires a natural-language user part; without one the backend
	// returns empty responses for tool-only turns.
	hasUserText := false
	for _, c := range contents {
		if c["role"] != "user" {
			continue
		}
		for _, p := range asSlice(c["parts"]) {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["text"].(string); strings.TrimSpace(t) != "" {
					hasUserText = true
				}
			}
		}
	}
	if !hasUserText && len(system) > 0 {
		contents = append([]map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "Apply the active system instructions."}},
		}}, contents...)
	}
	return system, contents, toolNames
}

func runtimeHint(in map[string]any) string {
	m, _ := in["model"].(string)
	return stripPrefix(m)
}

func appendTurn(contents *[]map[string]any, role string, parts []map[string]any) {
	if len(parts) == 0 {
		return
	}
	if n := len(*contents); n > 0 && (*contents)[n-1]["role"] == role {
		(*contents)[n-1]["parts"] = append(asSlice((*contents)[n-1]["parts"]), toAnySlice(parts)...)
		return
	}
	*contents = append(*contents, map[string]any{"role": role, "parts": toAnySlice(parts)})
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func stringContent(v any) string {
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
			switch typ {
			case "text":
				if s, _ := m["text"].(string); s != "" {
					b.WriteString(s)
				}
			case "input_text":
				if s, _ := m["text"].(string); s != "" {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func contentArrayTexts(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if t, _ := m["type"].(string); t == "text" || t == "input_text" {
			if s, _ := m["text"].(string); strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// userParts handles string + array content incl. image_url data URLs.
func userParts(content any) []map[string]any {
	switch t := content.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []map[string]any{{"text": t}}
	case []any:
		var out []map[string]any
		for _, item := range t {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				if s, _ := m["text"].(string); strings.TrimSpace(s) != "" {
					out = append(out, map[string]any{"text": s})
				}
			case "image_url":
				var urlStr string
				if um, ok := m["image_url"].(map[string]any); ok {
					urlStr, _ = um["url"].(string)
				} else if s, ok := m["image_url"].(string); ok {
					urlStr = s
				}
				if mime, data := parseDataURL(urlStr); data != "" {
					out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}})
				}
			}
		}
		return out
	default:
		return nil
	}
}

func parseDataURL(s string) (mime, data string) {
	if !strings.HasPrefix(s, "data:") {
		return "", ""
	}
	rest := strings.TrimPrefix(s, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", ""
	}
	mime = rest[:semi]
	data = strings.TrimSpace(rest[semi+len(";base64,"):])
	if data == "" {
		return "", ""
	}
	// Validate base64 quickly.
	if _, err := base64.StdEncoding.DecodeString(firstN(data, 64)); err != nil {
		if _, err := base64.RawStdEncoding.DecodeString(firstN(data, 64)); err != nil {
			// Still pass through — backend validates fully.
		}
	}
	return mimeOrDefault(mime), data
}

func firstN(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
	if len(s) > n {
		return s[:n]
	}
	return s
}

func mimeOrDefault(m string) string {
	if m == "" {
		return "image/png"
	}
	return m
}

func sanitizeID(id, fallback string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return fallback + "_1"
	}
	return s
}

func convertTools(in map[string]any) []map[string]any {
	raw, ok := in["tools"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	var decls []map[string]any
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			// Already Gemini-shaped? pass through.
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		decls = append(decls, map[string]any{
			"name":                 name,
			"description":          desc,
			"parametersJsonSchema": params,
		})
	}
	if len(decls) == 0 {
		return nil
	}
	return []map[string]any{{"functionDeclarations": decls}}
}

func convertToolChoice(in map[string]any) map[string]any {
	tc, ok := in["tool_choice"]
	if !ok || tc == nil {
		return nil
	}
	mode := "AUTO"
	switch v := tc.(type) {
	case string:
		switch v {
		case "none":
			mode = "NONE"
		case "required", "any":
			mode = "ANY"
		default:
			mode = "AUTO"
		}
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "none" {
			mode = "NONE"
		} else {
			mode = "AUTO"
		}
	}
	if mode == "AUTO" {
		return nil
	}
	return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
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

func firstFloat(in map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := in[k]; ok {
			if f, ok := toFloat(v); ok {
				return f
			}
		}
	}
	return 0
}
