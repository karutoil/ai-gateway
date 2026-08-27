package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// sanitizeOpenAICompatBody normalizes legal-but-sloppy OpenAI chat shapes that
// strict OpenAI-compatible upstreams (new-api style proxies fronting Claude
// with Zod request validation) reject with 400 "…: Invalid input" or a role
// discriminator error. The gateway relays upstream error bodies verbatim, so
// these shapes surface to clients as unfixed 400s even though the request is
// legal OpenAI.
//
// Evidence-backed rewrites (reproduced live against ckff.dev, see
// sanitize_test.go):
//  1. role:"tool" without a usable tool_call_id → synthesize one from name,
//     else a deterministic placeholder. Upstream validators require the key;
//     a synthesized id beats a guaranteed 400.
//  1b. tool content arrays may contain ONLY text parts — image parts and
//     unknown blocks 400. Images DO reach the model from a following user
//     message (verified live), so they are relocated into a synthetic user
//     message right after the tool message instead of being dropped. Unknown
//     non-image blocks become text placeholders on the tool message itself.
//  2. role:"developer" → "system" (same semantics; compat upstreams haven't
//     adopted the newer discriminator value).
//  3. assistant content:null with no tool_calls → drop the key. Missing
//     content is universally accepted; explicit null is rejected by some
//     dialects.
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

		// 2. developer → system
		if r, ok := m["role"]; ok && bytes.Equal(bytes.TrimSpace(r), []byte(`"developer"`)) {
			m["role"] = json.RawMessage(`"system"`)
			msgChanged = true
		}

		role := ""
		if r, ok := m["role"]; ok {
			_ = json.Unmarshal(r, &role)
		}

		// 1. tool message needs a non-empty string tool_call_id
		if role == "tool" {
			if needsToolCallID(m["tool_call_id"]) {
				if id, ok := stringValue(m["name"]); ok {
					m["tool_call_id"], _ = json.Marshal(id)
				} else {
					m["tool_call_id"], _ = json.Marshal(fmt.Sprintf("call_unnamed_%d", i))
				}
				msgChanged = true
			}
			// 1b. tool content: keep text, relocate images into a synthetic
			// user message appended right after this one.
			if tc, syn, did := splitToolContent(m["content"]); did {
				m["content"] = tc
				synthetic = syn
				msgChanged = true
			}
		}

		// 3. assistant content:null without tool_calls → drop the key
		if role == "assistant" {
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

// mustJSON marshals v or panics; only called with JSON-safe values built here.
func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}
