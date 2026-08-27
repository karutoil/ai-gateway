//go:build live

// Live E2E tests for Muse Spark 1.2 via real ckff provider.
// Run with: GATEWAY_URL=http://localhost:8989 go test -tags=live -run TestMuseLive -v ./internal/e2e
// Requires gateway running on GATEWAY_URL and provider ckff-muse configured with muse-spark-1.2-contributor.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	gwURL = envOr("GATEWAY_URL", "http://localhost:8989")
	model = envOr("MODEL", "muse-spark-1.2-contributor")
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return strings.TrimRight(v, "/")
	}
	return d
}

func gatewayKey(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("GATEWAY_KEY"); v != "" {
		return v
	}
	// Try to create a key via admin login
	token := adminToken(t)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("POST", gwURL+"/api/keys", bytes.NewReader([]byte(fmt.Sprintf(`{"name":"e2e-live-%d"}`, time.Now().Unix()))))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	if k, ok := m["key"].(string); ok && k != "" {
		return k
	}
	t.Fatalf("no key: %s", string(b))
	return ""
}

func adminToken(t *testing.T) string {
	t.Helper()
	pw := envOr("ADMIN_PASSWORD", "admin123")
	client := &http.Client{Timeout: 10 * time.Second}
	for _, body := range []string{
		fmt.Sprintf(`{"username":"admin","password":"%s"}`, pw),
		`{"password":"admin123"}`,
	} {
		resp, err := client.Post(gwURL+"/api/auth/login", "application/json", strings.NewReader(body))
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var m map[string]string
		json.Unmarshal(b, &m)
		if tok := m["token"]; tok != "" {
			return tok
		}
	}
	t.Fatalf("admin login failed")
	return ""
}

// Real tools - same as harness-muse for consistency
var openAITools = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get weather for a location",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"location": map[string]any{"type": "string"},
				},
				"required": []string{"location"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "calculate",
			"description": "Calculate expression",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"expression": map[string]any{"type": "string"},
				},
				"required": []string{"expression"},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "search_docs",
			"description": "Search docs",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required": []string{"query"},
			},
		},
	},
}

var anthropicTools = []map[string]any{
	{
		"name":        "get_weather",
		"description": "Get weather",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"location": map[string]any{"type": "string"}},
			"required":   []string{"location"},
		},
	},
}

func doJSON(t *testing.T, method, path string, key string, body any, expect int) []byte {
	t.Helper()
	client := &http.Client{Timeout: 45 * time.Second}
	var br io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		br = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, gwURL+path, br)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != expect {
		t.Fatalf("%s %s: expected %d got %d body %s", method, path, expect, resp.StatusCode, string(b[:min(800, len(b))]))
	}
	return b
}

func doAnthropic(t *testing.T, path string, key string, body any, expect int) []byte {
	t.Helper()
	client := &http.Client{Timeout: 45 * time.Second}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", gwURL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != expect {
		t.Fatalf("POST %s: expected %d got %d body %s", path, expect, resp.StatusCode, string(rb[:800]))
	}
	return rb
}

func doStream(t *testing.T, path string, key string, body any) {
	t.Helper()
	client := &http.Client{Timeout: 45 * time.Second}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", gwURL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream %s: expected 200 got %d %s", path, resp.StatusCode, string(rb[:500]))
	}
	buf := make([]byte, 8192)
	found := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 && (bytes.Contains(buf[:n], []byte("data:")) || bytes.Contains(buf[:n], []byte("event:"))) {
			found = true
			break
		}
		if err != nil {
			if found {
				break
			}
			if err == io.EOF {
				break
			}
			t.Fatalf("stream read: %v", err)
		}
	}
	if !found {
		t.Fatalf("no stream data for %s", path)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestMuseLive(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	gwKey := gatewayKey(t)
	t.Logf("Gateway: %s Model: %s Key: %s...", gwURL, model, gwKey[:12])
	isAnthropicModel := strings.Contains(strings.ToLower(model), "claude") || strings.Contains(strings.ToLower(model), "muse-spark") || strings.Contains(strings.ToLower(model), "muse")

	// Basic health
	t.Run("Health", func(t *testing.T) {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(gwURL + "/health")
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("health %d", resp.StatusCode)
		}
	})

	// Models
	t.Run("Models", func(t *testing.T) {
		doJSON(t, "GET", "/v1/models", gwKey, nil, 200)
	})

	// Chat completions — strict: anthropic models must 400 on OpenAI endpoints
	if isAnthropicModel {
		t.Run("ChatBasic", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "Say hi in one word"}}, "max_tokens": 20,
			}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
		t.Run("ChatStream", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true, "max_tokens": 20,
			}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error on stream, got %s", string(b[:min(500, len(b))]))
			}
		})
		t.Run("ChatTools", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "What is weather in Paris? Use get_weather."}},
				"tools": openAITools[:1], "tool_choice": "auto", "max_tokens": 200,
			}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
		t.Run("ChatReasoningLow", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "What is 2+2? Think step by step."}},
				"max_tokens": 100, "reasoning_effort": "low",
			}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
		t.Run("ChatReasoningMedium", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "What is 3+3?"}},
				"max_tokens": 100, "reasoning_effort": "medium",
			}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
	} else {
		t.Run("ChatBasic", func(t *testing.T) {
			doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "Say hi in one word"}}, "max_tokens": 20,
			}, 200)
		})
		t.Run("ChatStream", func(t *testing.T) {
			doStream(t, "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true, "max_tokens": 20,
			})
		})
		t.Run("ChatTools", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "What is weather in Paris? Use get_weather."}},
				"tools": openAITools[:1], "tool_choice": "auto", "max_tokens": 200,
			}, 200)
			if bytes.Contains(b, []byte("bad_response_status_code")) {
				t.Fatalf("tool translation failed: %s", string(b[:500]))
			}
		})
		t.Run("ChatReasoningLow", func(t *testing.T) {
			doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "What is 2+2? Think step by step."}},
				"max_tokens": 100, "reasoning_effort": "low",
			}, 200)
		})
		t.Run("ChatReasoningMedium", func(t *testing.T) {
			doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
				"model": model, "messages": []map[string]any{{"role": "user", "content": "What is 3+3?"}},
				"max_tokens": 100, "reasoning_effort": "medium",
			}, 200)
		})
	}
	t.Run("ChatReasoningInvalid", func(t *testing.T) {
		doJSON(t, "POST", "/v1/chat/completions", gwKey, map[string]any{
			"model": model, "messages": []map[string]any{{"role": "user", "content": "hi"}},
			"max_tokens": 10, "reasoning_effort": "ultra",
		}, 400)
	})

	// Anthropic
	t.Run("AnthropicBasic", func(t *testing.T) {
		doAnthropic(t, "/v1/messages", gwKey, map[string]any{
			"model": model, "max_tokens": 20, "messages": []map[string]any{{"role": "user", "content": "Say hi in one word"}},
		}, 200)
	})

	t.Run("AnthropicStream", func(t *testing.T) {
		client := &http.Client{Timeout: 45 * time.Second}
		body := map[string]any{"model": model, "max_tokens": 20, "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", gwURL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+gwKey)
		req.Header.Set("x-api-key", gwKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("anthropic stream: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			rb, _ := io.ReadAll(resp.Body)
			t.Fatalf("anthropic stream %d %s", resp.StatusCode, string(rb[:500]))
		}
	})

	t.Run("AnthropicTools", func(t *testing.T) {
		doAnthropic(t, "/v1/messages", gwKey, map[string]any{
			"model": model, "max_tokens": 100, "messages": []map[string]any{{"role": "user", "content": "Weather in Paris? Use get_weather."}},
			"tools": anthropicTools,
		}, 200)
	})

	// Responses — strict
	if isAnthropicModel {
		t.Run("ResponsesBasic", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/responses", gwKey, map[string]any{"model": model, "input": "Say hi in one word"}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
		t.Run("ResponsesInputText", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/responses", gwKey, map[string]any{
				"model": model,
				"input": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "Say hi"}}}},
			}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
		t.Run("ResponsesStream", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/responses", gwKey, map[string]any{"model": model, "input": "hi", "stream": true}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(500, len(b))]))
			}
		})
	} else {
		t.Run("ResponsesBasic", func(t *testing.T) {
			doJSON(t, "POST", "/v1/responses", gwKey, map[string]any{"model": model, "input": "Say hi in one word"}, 200)
		})
		t.Run("ResponsesInputText", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/responses", gwKey, map[string]any{
				"model": model,
				"input": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "Say hi"}}}},
			}, 200)
			if bytes.Contains(b, []byte("Invalid input")) {
				t.Fatalf("input_text failed: %s", string(b[:500]))
			}
		})
		t.Run("ResponsesStream", func(t *testing.T) {
			doStream(t, "/v1/responses", gwKey, map[string]any{"model": model, "input": "hi", "stream": true})
		})
	}
	// Legacy — strict: anthropic models must 400
	if isAnthropicModel {
		t.Run("Completions", func(t *testing.T) {
			b := doJSON(t, "POST", "/v1/completions", gwKey, map[string]any{"model": model, "prompt": "Hello", "max_tokens": 10}, 400)
			if !bytes.Contains(b, []byte("anthropic model")) {
				t.Fatalf("expected anthropic-model error, got %s", string(b[:min(400, len(b))]))
			}
		})
	} else {
		t.Run("Completions", func(t *testing.T) {
			client := &http.Client{Timeout: 45 * time.Second}
			body := map[string]any{"model": model, "prompt": "Hello", "max_tokens": 10}
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", gwURL+"/v1/completions", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+gwKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("completions: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == 404 {
				t.Fatalf("completions 404")
			}
		})
	}
	t.Run("ModelsNoAuth", func(t *testing.T) {
		client := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", gwURL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer sk-gw-invalid")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("no auth: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("expected 401 got %d", resp.StatusCode)
		}
	})
}
