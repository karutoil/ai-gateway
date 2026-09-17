package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

func toolCallsOf(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var top struct {
		Choices []struct {
			Message struct {
				ToolCalls []map[string]interface{} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	if len(top.Choices) == 0 {
		t.Fatal("no choices")
	}
	return top.Choices[0].Message.ToolCalls
}

// Live shape from Cognition SWE via LiteLLM proxy: one call's arguments
// split across empty-id fragment entries.
func TestMergeFragmentedToolCallsLiveShape(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"","tool_calls":[
		{"id":"read_file_0#abc","type":"function","function":{"name":"read_file","arguments":""}},
		{"id":"","type":"function","function":{"name":"","arguments":"{"}},
		{"id":"","type":"function","function":{"name":"","arguments":"\"path\": \""}},
		{"id":"","type":"function","function":{"name":"","arguments":"a"}},
		{"id":"","type":"function","function":{"name":"","arguments":".txt"}},
		{"id":"","type":"function","function":{"name":"","arguments":"\""}},
		{"id":"","type":"function","function":{"name":"","arguments":"}"}}],
		"finish_reason":"tool_calls"}}]}`)
	out, did := mergeFragmentedToolCalls(in)
	if !did {
		t.Fatal("expected a merge")
	}
	calls := toolCallsOf(t, out)
	if len(calls) != 1 {
		t.Fatalf("want 1 merged call, got %d: %v", len(calls), calls)
	}
	fn := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Fatalf("name = %v", fn["name"])
	}
	args, _ := fn["arguments"].(string)
	var js json.RawMessage
	if err := json.Unmarshal([]byte(args), &js); err != nil {
		t.Fatalf("merged arguments invalid JSON: %q: %v", args, err)
	}
	if calls[0]["id"] != "read_file_0#abc" {
		t.Fatalf("id lost: %v", calls[0]["id"])
	}
}

func TestMergeFragmentedToolCallsUntouchedWhenClean(t *testing.T) {
	for _, in := range []string{
		`{"id":"c","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
		`{"id":"c","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"g","arguments":"{\"x\":1}"}}]}}]}`,
		`not json`,
		`{"object":"list"}`,
	} {
		out, did := mergeFragmentedToolCalls([]byte(in))
		if did {
			t.Fatalf("must not touch clean body: %s", in)
		}
		if string(out) != in {
			t.Fatalf("byte change without merge flag: %s", in)
		}
	}
}

// Two parallel calls interleaved as head+fragments must regroup by id.
func TestMergeFragmentedToolCallsParallel(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"1","type":"function","function":{"name":"a","arguments":""}},
		{"id":"","type":"function","function":{"name":"","arguments":"{\"x\""}},
		{"id":"","type":"function","function":{"name":"","arguments":"}"}},
		{"id":"2","type":"function","function":{"name":"b","arguments":""}},
		{"id":"","type":"function","function":{"name":"","arguments":"[1]"}}]}}]}`)
	out, did := mergeFragmentedToolCalls(in)
	if !did {
		t.Fatal("expected a merge")
	}
	calls := toolCallsOf(t, out)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %+v", calls)
	}
	a1 := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	a2 := calls[1]["function"].(map[string]interface{})["arguments"].(string)
	if a1 != `{"x"}}` && a1 != `{"x"`+`}` {
		t.Fatalf("call1 args = %q", a1)
	}
	if a2 != "[1]" {
		t.Fatalf("call2 args = %q", a2)
	}
	for _, c := range calls {
		if c["id"] == "" {
			t.Fatalf("empty id survived: %+v", calls)
		}
	}
}

// An id-less orphan with no group head is kept, not invented.
func TestMergeFragmentedToolCallsOrphanKept(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"","type":"function","function":{"name":"","arguments":"half"}}]}}]}`)
	out, did := mergeFragmentedToolCalls(in)
	if did {
		t.Fatalf("orphan must not trigger a merge: %s", out)
	}
	if !strings.Contains(string(out), "half") {
		t.Fatalf("orphan content lost: %s", out)
	}
}

// End-to-end through the proxy: fragmented upstream tool_calls must reach
// the client merged into one valid call.
func TestProxyMergesFragmentedUpstreamToolCalls(t *testing.T) {
	srv, key := setupFragmentedUpstream(t)
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	respBody := postJSON(t, srv.URL+"/v1/chat/completions", key, body)
	calls := toolCallsOf(t, []byte(respBody))
	if len(calls) != 1 {
		t.Fatalf("want 1 merged call client-side, got %+v", calls)
	}
	fn := calls[0]["function"].(map[string]interface{})
	args, _ := fn["arguments"].(string)
	var js json.RawMessage
	if err := json.Unmarshal([]byte(args), &js); err != nil {
		t.Fatalf("client-side arguments invalid: %q", args)
	}
}

func setupFragmentedUpstream(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-frag","choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"f_1","type":"function","function":{"name":"read_file","arguments":""}},
			{"id":"","type":"function","function":{"name":"","arguments":"{\"path\""}},
			{"id":"","type":"function","function":{"name":"","arguments":":\"a.txt\"}"}}],
			"finish_reason":"tool_calls"}}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`))
	}))
	t.Cleanup(upstream.Close)
	if _, err := ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("frag-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, k.Key
}

func postJSON(t *testing.T, url, key, body string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	return string(b)
}
