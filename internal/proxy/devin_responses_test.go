package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/antigravity"
)

// Normalized tool calls must survive the Responses dialect: previously
// serveChunksAsResponses rendered text only and dropped every tool call, so
// /v1/responses agents on OAuth transports could never loop over tools.
func TestServeChunksAsResponsesKeepsToolCalls(t *testing.T) {
	h, _ := testDevinHandler(t)
	chunks := []antigravity.ParsedChunk{{
		Texts:     []string{"checking"},
		ToolCalls: []antigravity.ParsedToolCall{{ID: "call_1", Name: "list_dir", Arguments: map[string]any{"target_directory": "."}}},
		HasData:   true,
	}}

	w := httptest.NewRecorder()
	h.serveChunksAsResponses(w, chunks, "devin/swe-2", "responses", devinCostFor(), "test", "pid", time.Now(), false)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	found := false
	for _, item := range out.Output {
		if item.Type == "function_call" {
			found = true
			if item.CallID != "call_1" || item.Name != "list_dir" {
				t.Fatalf("function_call = %+v", item)
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(item.Arguments), &args); err != nil || args["target_directory"] != "." {
				t.Fatalf("arguments = %q", item.Arguments)
			}
		}
	}
	if !found {
		t.Fatalf("no function_call output item in %s", w.Body.String())
	}

	ws := httptest.NewRecorder()
	h.serveChunksAsResponses(ws, chunks, "devin/swe-2", "responses", devinCostFor(), "test", "pid", time.Now(), true)
	sse := ws.Body.String()
	for _, want := range []string{
		`"type":"function_call"`,
		`response.function_call_arguments.delta`,
		`response.function_call_arguments.done`,
		`list_dir`,
	} {
		if !strings.Contains(sse, want) {
			t.Fatalf("stream missing %q\n%s", want, sse)
		}
	}
}
