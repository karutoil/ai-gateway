package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// sanitizeOpenAICompatBody normalizes legal-but-sloppy OpenAI chat shapes that
// strict OpenAI-compatible upstreams (new-api style proxies fronting Claude
// with Zod request validation) reject with 400 "…: Invalid input", a role
// discriminator error, or silently drop. The gateway relays upstream error
// bodies verbatim, so these shapes surface to clients as unfixed 400s (or
// silent content loss) even though the request is legal OpenAI.
//
// Evidence-backed rewrites (each reproduced live against ckff.dev, see
// sanitize_test.go):
//  1. role:"tool" without a usable tool_call_id → synthesize one from name,
//     else a deterministic placeholder. Upstream validators require the key;
//     a synthesized id beats a guaranteed 400.
//     1b. tool content arrays accept ONLY text parts — image parts and unknown
//     blocks 400. Images DO reach the model from a following user message
//     (verified live), so they are relocated into a synthetic user message
//     right after the tool message instead of being dropped. Unknown
//     non-image blocks become text placeholders on the tool message itself.
//  2. role:"developer" → "system"; legacy role:"function" → "tool" (its name
//     field then feeds the tool_call_id synthesis in 1); any other unknown
//     role → "user" (a 400 "Invalid discriminator" is guaranteed worse).
//  3. assistant content:null with no tool_calls → drop the key. Missing
//     content is universally accepted; explicit null is rejected by some
//     dialects.
//  4. assistant tool_calls entries missing their function object → dropped
//     (orphaned tool results are accepted upstream); type:"custom" entries →
//     converted to function shape; function entries without arguments →
//     arguments:"{}".
//  5. content-array part fixes across all roles: Responses-style input_text →
//     text and input_image → image_url (upstream silently DROPS both, losing
//     content and 400ing "Prompt must contain at least one message" when
//     nothing remains); a part with text but no type → type:"text"; audio and
//     file parts (hard-rejected) → text placeholders.
//
// Anything unparseable — or a body without a messages array — is returned
// unchanged, so opaque or non-chat bodies relay exactly as before.
func sanitizeOpenAICompatBody(body []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	msgsRaw, ok := top["messages"]
	if !ok {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return body
	}

	changed := false
	outMsgs := make([]json.RawMessage, 0, len(msgs)+2)
	for i, mRaw := range msgs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(mRaw, &m); err != nil {
			outMsgs = append(outMsgs, mRaw)
			continue
		}
		msgChanged := false
		var synthetic json.RawMessage

		// 2. role fixes
		if r, ok := m["role"]; ok {
			switch role, _ := stringValue(r); role {
			case "system", "user", "assistant", "tool":
				// canonical — keep
			case "developer":
				m["role"] = json.RawMessage(`"system"`)
				msgChanged = true
			case "function":
				m["role"] = json.RawMessage(`"tool"`)
				msgChanged = true
			default:
				m["role"] = json.RawMessage(`"user"`)
				msgChanged = true
			}
		}
		role := ""
		if r, ok := m["role"]; ok {
			role, _ = stringValue(r)
		}

		// 5. content part fixes (all roles)
		if c, ok := m["content"]; ok {
			if nc, did := normalizeContentParts(c); did {
				m["content"] = nc
				msgChanged = true
			}
		}

		// 1/1b. tool message: id synthesis + image relocation
		if role == "tool" {
			if needsToolCallID(m["tool_call_id"]) {
				if id, ok := stringValue(m["name"]); ok {
					m["tool_call_id"], _ = json.Marshal(id)
				} else {
					m["tool_call_id"], _ = json.Marshal(fmt.Sprintf("call_unnamed_%d", i))
				}
				msgChanged = true
			}
			if tc, syn, did := splitToolContent(m["content"]); did {
				m["content"] = tc
				synthetic = syn
				msgChanged = true
			}
		}

		// 4/3. assistant message: tool_calls repair + content:null drop
		if role == "assistant" {
			if tc, ok := m["tool_calls"]; ok {
				if fixed, did := fixToolCalls(tc); did {
					if fixed == nil {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = fixed
					}
					msgChanged = true
				}
			}
			if c, ok := m["content"]; ok && bytes.Equal(bytes.TrimSpace(c), []byte(`null`)) {
				if _, hasCalls := m["tool_calls"]; !hasCalls {
					delete(m, "content")
					msgChanged = true
				}
			}
		}

		if msgChanged {
			if b, err := json.Marshal(m); err == nil {
				mRaw = b
			}
			changed = true
		}
		outMsgs = append(outMsgs, mRaw)
		if len(synthetic) > 0 {
			outMsgs = append(outMsgs, synthetic)
			changed = true
		}
	}

	if !changed {
		return body
	}
	msgsOut, err := json.Marshal(outMsgs)
	if err != nil {
		return body
	}
	top["messages"] = msgsOut
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// chatMessagesPresent reports whether the chat body carries a non-empty
// messages array. Strict upstreams answer a missing/empty array with an
// opaque 500 "field messages is required"; failing fast here gives clients a
// clean 400 with a standard error envelope instead. Only bodies that are not
// JSON objects at all are considered opaque (relayed as before).
func chatMessagesPresent(body []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return true // opaque body: let the existing pipeline deal with it
	}
	msgsRaw, ok := top["messages"]
	if !ok {
		return false
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return false // present but not an array
	}
	return len(msgs) > 0
}

// needsToolCallID reports whether the raw JSON value for tool_call_id is
// absent, null, non-string, or the empty string.
func needsToolCallID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	s, ok := stringValue(raw)
	if !ok {
		return true
	}
	return s == ""
}

// stringValue unmarshals a JSON string; returns ok=false for null/non-strings.
func stringValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// normalizeContentParts fixes content-array parts that strict upstreams
// reject or silently drop, across all roles:
//   - input_text → text (Responses-style alias; silently dropped otherwise)
//   - input_image / Anthropic image → image_url (silently dropped otherwise)
//   - part with text but no type → type:"text" (silently dropped otherwise)
//   - audio/file parts → text placeholders (hard 400 "content: Invalid input")
//   - an array that ends up (or starts) empty → one empty text part, since
//     empty content 400s "Prompt must contain at least one message"
//
// String content and non-arrays pass through unchanged. Unknown part types
// observed to be tolerated upstream are left as-is to keep valid bodies
// byte-identical.
func normalizeContentParts(raw json.RawMessage) (json.RawMessage, bool) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || parts == nil {
		return raw, false
	}
	changed := false
	for j, p := range parts {
		typ, _ := stringValue(p["type"])
		switch typ {
		case "text", "image_url":
			// canonical — keep
		case "input_text":
			p["type"] = json.RawMessage(`"text"`)
			changed = true
		case "input_image", "image":
			if norm, ok := normalizeImagePart(p); ok {
				parts[j] = norm
			} else {
				parts[j] = omittedPart("image")
			}
			changed = true
		case "input_audio", "audio":
			parts[j] = omittedPart("audio")
			changed = true
		case "file":
			parts[j] = omittedPart("file")
			changed = true
		default:
			// No type at all but a text field: upstream drops the part;
			// re-typing it preserves the content.
			if _, hasText := p["text"]; hasText {
				p["type"] = json.RawMessage(`"text"`)
				changed = true
			}
			// Other unknown types are tolerated upstream — leave as-is.
		}
	}
	if len(parts) == 0 {
		// A natively-empty content array (any role) is rejected upstream
		// ("Prompt must contain at least one message"). null content never
		// reaches here (unmarshal of null yields a nil slice, handled above).
		empty := []map[string]json.RawMessage{{
			"type": json.RawMessage(`"text"`),
			"text": json.RawMessage(`""`),
		}}
		if out, err := json.Marshal(empty); err == nil {
			return out, true
		}
		return raw, false
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(parts)
	if err != nil {
		return raw, false
	}
	return out, true
}

// omittedPart builds a text placeholder replacing an attachment type the
// upstream rejects outright.
func omittedPart(kind string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"type": json.RawMessage(`"text"`),
		"text": mustJSON(fmt.Sprintf("[%s attachment omitted: not supported by this provider]", kind)),
	}
}

// splitToolContent normalizes a tool message's raw content value.
//
// Strict upstreams validate tool content as string | text-part[]; image parts
// and unknown blocks (e.g. Anthropic tool_result leaks) 400 with "content:
// Invalid input". Verified live: the SAME image carried in a user message
// immediately after the tool result reaches the model, and consecutive user
// messages are accepted. So:
//   - string content, null, and text-only arrays pass through unchanged.
//   - image parts are stripped out of the tool message and returned as a
//     synthetic user message (marker text + normalized image_url parts).
//   - unknown non-image blocks become text placeholders on the tool message.
//
// Returns (toolContent, syntheticUserMessage, changed).
func splitToolContent(raw json.RawMessage) (json.RawMessage, json.RawMessage, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte(`null`)) {
		return raw, nil, false
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, nil, false // string or non-array: already valid
	}

	var keep []map[string]json.RawMessage
	var images []map[string]json.RawMessage
	unknown := false
	for _, p := range parts {
		typ, _ := stringValue(p["type"])
		switch typ {
		case "text", "":
			keep = append(keep, p)
		case "image_url", "image", "input_image":
			images = append(images, p)
		default:
			// Anthropic-style leaks (tool_result, tool_use, …) and anything
			// unrecognized: preserve a trace as text so context isn't lost.
			keep = append(keep, map[string]json.RawMessage{
				"type": json.RawMessage(`"text"`),
				"text": mustJSON(fmt.Sprintf("[non-text part %q omitted from tool result]", typ)),
			})
			unknown = true
		}
	}
	if len(images) == 0 && !unknown {
		return raw, nil, false
	}

	if len(keep) == 0 {
		// An empty content array is itself invalid ("Prompt must contain at
		// least one message" family); keep a breadcrumb on the tool message.
		keep = append(keep, map[string]json.RawMessage{
			"type": json.RawMessage(`"text"`),
			"text": json.RawMessage(`"[image attached in the following message]"`),
		})
	}
	toolContent, err := json.Marshal(keep)
	if err != nil {
		return raw, nil, false
	}

	if len(images) == 0 {
		return toolContent, nil, true
	}

	userParts := []map[string]json.RawMessage{{
		"type": json.RawMessage(`"text"`),
		"text": json.RawMessage(`"[image from the preceding tool result]"`),
	}}
	for _, img := range images {
		if norm, ok := normalizeImagePart(img); ok {
			userParts = append(userParts, norm)
		}
	}
	synthetic, err := json.Marshal(map[string]json.RawMessage{
		"role":    json.RawMessage(`"user"`),
		"content": mustJSON(userParts),
	})
	if err != nil {
		return toolContent, nil, true
	}
	return toolContent, synthetic, true
}

// normalizeImagePart converts the image-ish part variants seen in the wild
// (OpenAI image_url object/string, Responses input_image, Anthropic image
// source) into a standard {"type":"image_url","image_url":{"url":…}} part.
func normalizeImagePart(p map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	if v, ok := p["image_url"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return map[string]json.RawMessage{
				"type":      json.RawMessage(`"image_url"`),
				"image_url": mustJSON(map[string]string{"url": s}),
			}, true
		}
		return map[string]json.RawMessage{
			"type":      json.RawMessage(`"image_url"`),
			"image_url": v,
		}, true
	}
	// Anthropic-style: {"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}
	if src, ok := p["source"]; ok {
		var s struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		}
		if err := json.Unmarshal(src, &s); err == nil && s.Data != "" && s.MediaType != "" {
			return map[string]json.RawMessage{
				"type":      json.RawMessage(`"image_url"`),
				"image_url": mustJSON(map[string]string{"url": "data:" + s.MediaType + ";base64," + s.Data}),
			}, true
		}
	}
	// Unrecognized image variant: drop rather than forward an invalid part.
	return nil, false
}

// fixToolCalls repairs assistant tool_calls arrays:
//   - entries with type:"custom" (OpenAI custom tools) → function shape with
//     the custom tool's name and empty arguments
//   - entries whose function object is missing or nameless → dropped
//     (orphaned tool results are accepted upstream, verified live)
//   - function entries without arguments → arguments:"{}"
//
// Returns (nil, true) when every entry was dropped and the key should be
// removed entirely.
func fixToolCalls(raw json.RawMessage) (json.RawMessage, bool) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil || calls == nil {
		return raw, false
	}
	changed := false
	out := make([]map[string]json.RawMessage, 0, len(calls))
	for _, c := range calls {
		typ, _ := stringValue(c["type"])
		if fn, ok := c["custom"]; ok && (typ == "custom" || typ == "") {
			name, _ := stringValue(fn)
			if name == "" {
				var cm map[string]json.RawMessage
				if json.Unmarshal(fn, &cm) == nil {
					name, _ = stringValue(cm["name"])
				}
			}
			if name == "" {
				changed = true
				continue
			}
			out = append(out, map[string]json.RawMessage{
				"id":   c["id"],
				"type": json.RawMessage(`"function"`),
				"function": mustJSON(map[string]json.RawMessage{
					"name":      mustJSON(name),
					"arguments": json.RawMessage(`"{}"`),
				}),
			})
			changed = true
			continue
		}
		fnRaw, hasFn := c["function"]
		if !hasFn {
			changed = true
			continue // entry without function object: guaranteed 400 upstream
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(fnRaw, &fn) != nil {
			changed = true
			continue
		}
		name, _ := stringValue(fn["name"])
		if name == "" {
			changed = true
			continue
		}
		// A tool_call without an id leaves the following tool result
		// unmatchable; synthesize a deterministic one.
		if needsToolCallID(c["id"]) {
			c["id"] = mustJSON(fmt.Sprintf("call_%d", len(out)))
			changed = true
		}
		if _, hasArgs := fn["arguments"]; !hasArgs {
			fn["arguments"] = json.RawMessage(`"{}"`)
			c["function"] = mustJSON(fn)
			changed = true
		}
		out = append(out, c)
	}
	if !changed {
		return raw, false
	}
	if len(out) == 0 {
		return nil, true
	}
	return mustJSON(out), true
}

// mustJSON marshals v or returns an empty JSON string on failure; only called
// with JSON-safe values built here.
func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}
