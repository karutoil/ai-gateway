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
	for i, mRaw := range msgs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(mRaw, &m); err != nil {
			continue
		}
		msgChanged := false

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
			b, err := json.Marshal(m)
			if err != nil {
				continue // keep original message on re-marshal failure
			}
			msgs[i] = b
			changed = true
		}
	}

	if !changed {
		return body
	}
	msgsOut, err := json.Marshal(msgs)
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
