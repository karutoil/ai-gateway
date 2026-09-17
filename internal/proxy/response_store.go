package proxy

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/translate"
)

const (
	// responseHistoryTTL bounds how long a translated /v1/responses turn
	// stays chainable via previous_response_id. Conversations are
	// interactive sessions, not archives; a day covers long agent runs
	// while keeping the table small.
	responseHistoryTTL = 24 * time.Hour
	// responseHistoryMaxMessages caps one expanded history. Beyond this
	// the client must send the full conversation itself.
	responseHistoryMaxMessages = 200
	// responseHistoryMaxBytes caps the stored JSON per turn.
	responseHistoryMaxBytes = 1 << 20
)

// extractPreviousResponseID returns the previous_response_id carried by a
// /v1/responses body, or "" when the turn starts a new conversation.
func extractPreviousResponseID(body []byte) string {
	var probe struct {
		Prev *string `json:"previous_response_id"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.Prev == nil {
		return ""
	}
	return strings.TrimSpace(*probe.Prev)
}

// chatMessageList extracts the raw messages array from a chat-shaped body.
func chatMessageList(body []byte) []json.RawMessage {
	var top struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &top) != nil {
		return nil
	}
	return top.Messages
}

// assistantHistoryMessage folds translated-responses output items back into
// chat history: the assistant message text plus one assistant tool_calls
// entry per function_call item, mirroring the shape ResponsesToChat
// produces in the input direction so pairs line up on the next turn.
// Returns nil when the output carries nothing worth remembering.
func assistantHistoryMessage(outBody []byte) json.RawMessage {
	var resp struct {
		Output []map[string]interface{} `json:"output"`
	}
	if json.Unmarshal(outBody, &resp) != nil || len(resp.Output) == 0 {
		return nil
	}
	var text strings.Builder
	var calls []map[string]interface{}
	for _, item := range resp.Output {
		t, _ := item["type"].(string)
		switch t {
		case "message":
			text.WriteString(outputItemText(item))
		case "function_call":
			name, _ := item["name"].(string)
			if name == "" {
				name = "unknown_function"
			}
			args, _ := item["arguments"].(string)
			if args == "" {
				args = "{}"
			}
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			calls = append(calls, map[string]interface{}{
				"id":       callID,
				"type":     "function",
				"function": map[string]interface{}{"name": name, "arguments": args},
			})
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil
	}
	msg := map[string]interface{}{"role": "assistant"}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return raw
}

// outputItemText pulls plain text out of a Responses message output item.
func outputItemText(item map[string]interface{}) string {
	var b strings.Builder
	content, _ := item["content"].([]interface{})
	for _, part := range content {
		pm, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		switch pm["type"] {
		case "output_text", "text", "input_text":
			if s, ok := pm["text"].(string); ok {
				b.WriteString(s)
			}
		case "refusal":
			if s, ok := pm["refusal"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	// Some upstreams emit message content as a bare string.
	if s, ok := item["content"].(string); ok {
		b.WriteString(s)
	}
	return b.String()
}

// saveResponseTurn records one translated turn's full chat history under the
// response id issued to the client, so the next turn can chain with
// previous_response_id. Failures are silent: history is best-effort, and a
// store outage must not fail an otherwise good completion.
func (h *Handler) saveResponseTurn(prevID, respID, keyPrefix, providerID, model string, messages []json.RawMessage) {
	if h.DB == nil || respID == "" || len(messages) == 0 {
		return
	}
	if len(messages) > responseHistoryMaxMessages {
		messages = messages[len(messages)-responseHistoryMaxMessages:]
	}
	raw, err := json.Marshal(messages)
	if err != nil || len(raw) == 0 || len(raw) > responseHistoryMaxBytes {
		return
	}
	now := time.Now().Unix()
	_, _ = h.DB.Exec(db.Q(`INSERT INTO response_turns(id, key_prefix, provider_id, model, prev_id, messages, created_at_unix) VALUES(?,?,?,?,?,?,?)`),
		respID, keyPrefix, providerID, model, prevID, string(raw), now)
	// Opportunistic expiry; indexed integer comparison, cheap enough per turn.
	_, _ = h.DB.Exec(db.Q(`DELETE FROM response_turns WHERE created_at_unix < ?`), now-int64(responseHistoryTTL/time.Second))
}

// loadResponseHistory returns the stored chat history for a chained turn,
// key-scoped so one gateway key can never pick up another's conversation.
func (h *Handler) loadResponseHistory(prevID, keyPrefix string) ([]translate.OpenAIMessage, error) {
	if h.DB == nil {
		return nil, sql.ErrNoRows
	}
	var raw string
	var created int64
	err := h.DB.QueryRow(db.Q(`SELECT messages, created_at_unix FROM response_turns WHERE id=? AND key_prefix=?`),
		prevID, keyPrefix).Scan(&raw, &created)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix()-created > int64(responseHistoryTTL/time.Second) {
		return nil, sql.ErrNoRows
	}
	var msgs []translate.OpenAIMessage
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, err
	}
	if len(msgs) > responseHistoryMaxMessages {
		return nil, sql.ErrNoRows
	}
	return msgs, nil
}

// mergeResponseHistory prepends stored history to freshly translated chat
// messages. When the new turn carries its own instructions, the history's
// leading system prompt is dropped in favor of the explicit per-turn one.
func mergeResponseHistory(history, fresh []translate.OpenAIMessage) []translate.OpenAIMessage {
	if len(history) == 0 {
		return fresh
	}
	if len(fresh) > 0 && strings.EqualFold(strings.TrimSpace(fresh[0].Role), "system") {
		for len(history) > 0 && strings.EqualFold(strings.TrimSpace(history[0].Role), "system") {
			history = history[1:]
		}
	}
	return append(history, fresh...)
}

// storeTranslatedTurn persists one translated turn for previous_response_id
// chaining: the chat messages sent upstream plus the assistant's reply
// folded back from the Responses output, keyed by the issued response id.
func (h *Handler) storeTranslatedTurn(prevID, keyPrefix, providerID, model string, chatBody, responsesBody []byte) {
	var idProbe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(responsesBody, &idProbe) != nil || idProbe.ID == "" {
		return
	}
	msgs := chatMessageList(chatBody)
	if len(msgs) == 0 {
		return
	}
	if reply := assistantHistoryMessage(responsesBody); reply != nil {
		msgs = append(msgs, reply)
	}
	h.saveResponseTurn(prevID, idProbe.ID, keyPrefix, providerID, model, msgs)
}

// storeStreamedTurn persists one translated STREAMING turn: the chat
// messages sent upstream plus the assistant's accumulated text and tool
// calls, keyed by the streamed response id.
func (h *Handler) storeStreamedTurn(prevID, keyPrefix, providerID, model string, chatBody []byte, res responsesPumpResult) {
	if res.respID == "" {
		return
	}
	msgs := chatMessageList(chatBody)
	if len(msgs) == 0 {
		return
	}
	output := []map[string]interface{}{{
		"type":    "message",
		"content": []map[string]interface{}{{"type": "output_text", "text": res.text}},
	}}
	for _, tc := range res.toolCalls {
		args := tc.Args
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"call_id":   tc.ID,
			"name":      tc.Name,
			"arguments": args,
		})
	}
	synth, err := json.Marshal(map[string]interface{}{"output": output})
	if err != nil {
		return
	}
	if reply := assistantHistoryMessage(synth); reply != nil {
		msgs = append(msgs, reply)
	}
	h.saveResponseTurn(prevID, res.respID, keyPrefix, providerID, model, msgs)
}
