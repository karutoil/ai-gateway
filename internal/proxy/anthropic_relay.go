package proxy

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// anthToOpenAICompletionStream rewrites anthropic-dialect SSE (message_start /
// content_block_delta / message_delta / message_stop) into LEGACY OpenAI
// completion chunks for /v1/completions callers that were routed onto an
// anthropic upstream by the name/model heuristics. Without it those clients
// received foreign frames with no [DONE] sentinel and no finish_reason.
//
// Usage/token extraction is NOT this type's job: pumpStream keeps harvesting
// raw frames via its own harvest() path; consume() only produces relay bytes.
type anthToOpenAICompletionStream struct {
	model    string
	id       string
	created  int64
	text     strings.Builder
	stop     string // mapped stop_reason once message_delta arrives
	doneSent bool
}

func newAnthToOpenAICompletionStream(model string) *anthToOpenAICompletionStream {
	return &anthToOpenAICompletionStream{
		model:   model,
		id:      "cmpl-" + uuid.NewString()[:8],
		created: time.Now().Unix(),
	}
}

func (s *anthToOpenAICompletionStream) frame(delta map[string]interface{}, finish interface{}) []byte {
	chunk := map[string]interface{}{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			},
		},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

var doneFrame = []byte("data: [DONE]\n\n")

// consume ingests raw upstream bytes, extracts complete SSE frames, and
// returns equivalent OpenAI completion-chunk bytes (possibly empty).
func (s *anthToOpenAICompletionStream) consume(raw []byte) []byte {
	var out []byte
	for _, ev := range parseSSEEvents(raw) {
		switch ev.name {
		case "content_block_delta", "":
			// text deltas arrive as content_block_delta events; tolerate
			// data-only framing.
			var m map[string]interface{}
			if json.Unmarshal(ev.data, &m) != nil {
				continue
			}
			if t, _ := m["type"].(string); t == "ping" || t == "" {
				continue
			}
			if delta, ok := m["delta"].(map[string]interface{}); ok {
				if piece, ok := delta["text"].(string); ok && piece != "" {
					s.text.WriteString(piece)
					if f := s.frame(map[string]interface{}{"content": piece}, nil); f != nil {
						out = append(out, f...)
					}
				}
			}
		case "message_delta":
			var m map[string]interface{}
			if json.Unmarshal(ev.data, &m) != nil {
				continue
			}
			if d, ok := m["delta"].(map[string]interface{}); ok {
				if sr, exists := d["stop_reason"]; exists {
					s.stop = mapStopReason(sr)
				}
			}
		case "message_stop":
			s.emitDone(&out)
		}
	}
	return out
}

// emitDone writes the terminal finish_reason chunk plus the [DONE] sentinel,
// exactly once.
func (s *anthToOpenAICompletionStream) emitDone(out *[]byte) {
	if s.doneSent {
		return
	}
	s.doneSent = true
	stop := s.stop
	if stop == "" {
		stop = "stop"
	}
	if f := s.frame(map[string]interface{}{}, stop); f != nil {
		*out = append(*out, f...)
	}
	*out = append(*out, doneFrame...)
}

// finish flushes the terminal frames when the stream ended without an
// explicit message_stop (EOF / cut): guarantees OpenAI clients always see a
// finish_reason + [DONE] pair after any content was streamed.
func (s *anthToOpenAICompletionStream) finish() []byte {
	var out []byte
	if s.doneSent {
		return out
	}
	// Always close the protocol properly, even for empty generations.
	s.emitDone(&out)
	return out
}
